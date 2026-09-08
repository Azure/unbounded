// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Wire framing for the untagged `FI_EP_MSG` transport.
//!
//! The RDM transport multiplexed message kind and request id into the
//! 64-bit tag of each tagged send. The connection-oriented MSG endpoint
//! has no tags, so every message instead carries an explicit 8-byte
//! [`MsgHeader`] prefix: a one-byte [`MsgKind`] discriminant, three
//! padding bytes, and a little-endian `u32` request id. The receiver
//! reads this prefix to demultiplex (which is this message?) and to
//! correlate acknowledgments back to the originating request stream.
//!
//! The header is serialized by hand (not via serde) so its size and
//! layout are fixed and cheap to parse in the progress-thread hot path.

use super::error::{FabricError, Result};

/// Fixed on-wire size of a [`MsgHeader`]: `kind` (1) + padding (3) +
/// `request_id` (4).
pub(crate) const MSG_HEADER_LEN: usize = 8;

/// Size of each pre-posted receive buffer in the per-connection recv
/// pool. Must hold the largest control message: a request
/// ([`MsgHeader`] + `RequestHeader` + a serialized request body). Sized
/// to match the RDM transport's former request recv buffer (64 KiB).
pub(crate) const RECV_BUF_LEN: usize = 64 * 1024;

/// Message kind, carried in the first byte of every framed message.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub(crate) enum MsgKind {
    /// Client -> server: a `RequestHeader` followed by a serialized
    /// request body. Triggers an RMA write of the requested pages back
    /// to the client, each followed by a `PageAck`.
    Request = 1,
    /// Server -> client: one `PageAck` per page successfully written
    /// into the client's destination memory region.
    PageAck = 2,
    /// Server -> client: end-of-stream marker sent when a response
    /// completes with fewer pages than the client reserved (a fully
    /// satisfied fixed-size response omits it).
    ResponseEnd = 3,
    /// Server -> client: a serialized `ErrorAck` terminating the stream
    /// with a server-side error.
    ErrorAck = 4,
}

impl MsgKind {
    /// Map the on-wire discriminant byte back to a [`MsgKind`].
    pub(crate) fn from_u8(v: u8) -> Result<Self> {
        match v {
            1 => Ok(MsgKind::Request),
            2 => Ok(MsgKind::PageAck),
            3 => Ok(MsgKind::ResponseEnd),
            4 => Ok(MsgKind::ErrorAck),
            _ => Err(FabricError::NotFound("unknown MsgKind discriminant")),
        }
    }
}

/// The fixed 8-byte prefix on every framed message.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct MsgHeader {
    pub(crate) kind: MsgKind,
    pub(crate) request_id: u32,
}

impl MsgHeader {
    pub(crate) fn new(kind: MsgKind, request_id: u32) -> Self {
        MsgHeader { kind, request_id }
    }

    /// Serialize into the first [`MSG_HEADER_LEN`] bytes of `out`.
    /// Panics if `out` is too short (callers always size buffers to at
    /// least `MSG_HEADER_LEN`).
    pub(crate) fn write_to(&self, out: &mut [u8]) {
        assert!(out.len() >= MSG_HEADER_LEN, "header buffer too small");
        out[0] = self.kind as u8;
        out[1] = 0;
        out[2] = 0;
        out[3] = 0;
        out[4..8].copy_from_slice(&self.request_id.to_le_bytes());
    }

    /// Parse the prefix from `buf`. Errors if `buf` is shorter than the
    /// header or the kind byte is unrecognized.
    pub(crate) fn read_from(buf: &[u8]) -> Result<Self> {
        if buf.len() < MSG_HEADER_LEN {
            return Err(FabricError::NotFound("message shorter than MsgHeader"));
        }
        let kind = MsgKind::from_u8(buf[0])?;
        let mut rid = [0u8; 4];
        rid.copy_from_slice(&buf[4..8]);
        Ok(MsgHeader {
            kind,
            request_id: u32::from_le_bytes(rid),
        })
    }

    /// Build a framed message: an [`MSG_HEADER_LEN`]-byte prefix
    /// followed by `body`. Used by the small-message (control) send
    /// paths.
    pub(crate) fn frame(kind: MsgKind, request_id: u32, body: &[u8]) -> Vec<u8> {
        let mut out = vec![0u8; MSG_HEADER_LEN + body.len()];
        MsgHeader::new(kind, request_id).write_to(&mut out);
        out[MSG_HEADER_LEN..].copy_from_slice(body);
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn header_round_trips_through_bytes() {
        for (kind, rid) in [
            (MsgKind::Request, 0u32),
            (MsgKind::PageAck, 1),
            (MsgKind::ResponseEnd, 0xDEAD_BEEF),
            (MsgKind::ErrorAck, u32::MAX),
        ] {
            let mut buf = [0u8; MSG_HEADER_LEN];
            MsgHeader::new(kind, rid).write_to(&mut buf);
            let got = MsgHeader::read_from(&buf).expect("parse");
            assert_eq!(got.kind, kind);
            assert_eq!(got.request_id, rid);
        }
    }

    #[test]
    fn frame_prefixes_body() {
        let body = [0xAAu8, 0xBB, 0xCC];
        let framed = MsgHeader::frame(MsgKind::PageAck, 7, &body);
        assert_eq!(framed.len(), MSG_HEADER_LEN + 3);
        let h = MsgHeader::read_from(&framed).expect("parse");
        assert_eq!(h.kind, MsgKind::PageAck);
        assert_eq!(h.request_id, 7);
        assert_eq!(&framed[MSG_HEADER_LEN..], &body);
    }

    #[test]
    fn unknown_kind_is_rejected() {
        assert!(MsgKind::from_u8(0).is_err());
        assert!(MsgKind::from_u8(5).is_err());
        let buf = [9u8; MSG_HEADER_LEN];
        assert!(MsgHeader::read_from(&buf).is_err());
    }

    #[test]
    fn short_buffer_is_rejected() {
        let buf = [0u8; MSG_HEADER_LEN - 1];
        assert!(MsgHeader::read_from(&buf).is_err());
    }
}
