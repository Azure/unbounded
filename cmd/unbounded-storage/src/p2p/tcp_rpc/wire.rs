// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Network-byte-order frame and metadata codecs for TCP RPC.

use std::error::Error;
use std::fmt;

pub const MAGIC: [u8; 4] = *b"UBRP";
pub const VERSION: u8 = 1;
pub const HEADER_LEN: usize = 24;
pub const MAX_METADATA_LEN: usize = 64 * 1024;
pub const MAX_REQUEST_BYTES: usize = 64 * 1024;
pub const MAX_PAGE_BODY_LEN: usize = 16 * 1024 * 1024;
pub const MAX_PEER_NAME_LEN: usize = 255;
pub const MAX_ERROR_MESSAGE_LEN: usize = 4096;
pub const MAX_DESTINATION_PAGE_COUNT: u32 = u16::MAX as u32;

const SUPPORTED_FLAGS: u16 = 0;
const HANDSHAKE_FIXED_LEN: usize = 10;
const REQUEST_METADATA_LEN: usize = 56;
const PAGE_METADATA_LEN: usize = 12;
const ERROR_FIXED_LEN: usize = 6;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u8)]
pub enum FrameKind {
    Handshake = 1,
    Request = 2,
    Page = 3,
    End = 4,
    Error = 5,
    Cancel = 6,
}

impl FrameKind {
    fn from_u8(value: u8) -> Result<Self, WireError> {
        match value {
            1 => Ok(Self::Handshake),
            2 => Ok(Self::Request),
            3 => Ok(Self::Page),
            4 => Ok(Self::End),
            5 => Ok(Self::Error),
            6 => Ok(Self::Cancel),
            _ => Err(WireError::UnknownKind(value)),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct FrameHeader {
    pub kind: FrameKind,
    pub flags: u16,
    pub metadata_len: u32,
    pub payload_len: u32,
    pub request_id: u64,
}

impl FrameHeader {
    pub fn new(
        kind: FrameKind,
        request_id: u64,
        metadata_len: usize,
        payload_len: usize,
    ) -> Result<Self, WireError> {
        let metadata_len =
            u32::try_from(metadata_len).map_err(|_| WireError::MetadataTooLarge(metadata_len))?;
        let payload_len =
            u32::try_from(payload_len).map_err(|_| WireError::PayloadTooLarge(payload_len))?;
        let header = Self {
            kind,
            flags: 0,
            metadata_len,
            payload_len,
            request_id,
        };
        header.validate()?;
        Ok(header)
    }

    pub fn encode(self) -> Result<[u8; HEADER_LEN], WireError> {
        self.validate()?;
        let mut out = [0; HEADER_LEN];
        out[0..4].copy_from_slice(&MAGIC);
        out[4] = VERSION;
        out[5] = self.kind as u8;
        out[6..8].copy_from_slice(&self.flags.to_be_bytes());
        out[8..12].copy_from_slice(&self.metadata_len.to_be_bytes());
        out[12..16].copy_from_slice(&self.payload_len.to_be_bytes());
        out[16..24].copy_from_slice(&self.request_id.to_be_bytes());
        Ok(out)
    }

    pub fn decode(input: &[u8]) -> Result<DecodeStatus<Self>, WireError> {
        if input.len() < HEADER_LEN {
            return Ok(DecodeStatus::Incomplete {
                needed: HEADER_LEN - input.len(),
            });
        }
        if input[0..4] != MAGIC {
            return Err(WireError::InvalidMagic);
        }
        if input[4] != VERSION {
            return Err(WireError::UnsupportedVersion(input[4]));
        }

        let header = Self {
            kind: FrameKind::from_u8(input[5])?,
            flags: u16::from_be_bytes([input[6], input[7]]),
            metadata_len: u32::from_be_bytes(input[8..12].try_into().unwrap()),
            payload_len: u32::from_be_bytes(input[12..16].try_into().unwrap()),
            request_id: u64::from_be_bytes(input[16..24].try_into().unwrap()),
        };
        header.validate()?;
        Ok(DecodeStatus::Complete {
            value: header,
            consumed: HEADER_LEN,
        })
    }

    pub fn prefix_len(self) -> Result<usize, WireError> {
        checked_add(HEADER_LEN, self.metadata_len as usize)
    }

    pub fn frame_len(self) -> Result<usize, WireError> {
        checked_add(self.prefix_len()?, self.payload_len as usize)
    }

    fn validate(self) -> Result<(), WireError> {
        if self.flags & !SUPPORTED_FLAGS != 0 {
            return Err(WireError::UnsupportedFlags(self.flags));
        }

        let metadata_len = self.metadata_len as usize;
        let payload_len = self.payload_len as usize;
        if metadata_len > MAX_METADATA_LEN {
            return Err(WireError::MetadataTooLarge(metadata_len));
        }

        match self.kind {
            FrameKind::Handshake => {
                require_request_id(self.kind, self.request_id, true)?;
                require_metadata_range(
                    self.kind,
                    metadata_len,
                    HANDSHAKE_FIXED_LEN,
                    HANDSHAKE_FIXED_LEN + MAX_PEER_NAME_LEN,
                )?;
                require_payload_len(self.kind, payload_len, 0)?;
            }
            FrameKind::Request => {
                require_request_id(self.kind, self.request_id, false)?;
                require_metadata_len(self.kind, metadata_len, REQUEST_METADATA_LEN)?;
                if payload_len > MAX_REQUEST_BYTES {
                    return Err(WireError::PayloadTooLarge(payload_len));
                }
            }
            FrameKind::Page => {
                require_request_id(self.kind, self.request_id, false)?;
                require_metadata_len(self.kind, metadata_len, PAGE_METADATA_LEN)?;
                if payload_len == 0 || payload_len > MAX_PAGE_BODY_LEN {
                    return Err(WireError::InvalidPayloadLength {
                        kind: self.kind,
                        actual: payload_len,
                    });
                }
            }
            FrameKind::End | FrameKind::Cancel => {
                require_request_id(self.kind, self.request_id, false)?;
                require_metadata_len(self.kind, metadata_len, 0)?;
                require_payload_len(self.kind, payload_len, 0)?;
            }
            FrameKind::Error => {
                require_request_id(self.kind, self.request_id, false)?;
                require_metadata_range(
                    self.kind,
                    metadata_len,
                    ERROR_FIXED_LEN,
                    ERROR_FIXED_LEN + MAX_ERROR_MESSAGE_LEN,
                )?;
                require_payload_len(self.kind, payload_len, 0)?;
            }
        }

        self.frame_len()?;
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Handshake {
    pub peer_name: String,
    pub lane_index: u16,
    pub lane_count: u16,
    pub max_page: u32,
}

impl Handshake {
    pub fn encode_metadata(&self) -> Result<Vec<u8>, WireError> {
        self.validate()?;
        let name = self.peer_name.as_bytes();
        let mut out = Vec::with_capacity(HANDSHAKE_FIXED_LEN + name.len());
        put_u16(&mut out, name.len() as u16);
        out.extend_from_slice(name);
        put_u16(&mut out, self.lane_index);
        put_u16(&mut out, self.lane_count);
        put_u32(&mut out, self.max_page);
        Ok(out)
    }

    pub fn decode_metadata(input: &[u8]) -> Result<Self, WireError> {
        let mut reader = Reader::new(input);
        let name_len = reader.u16()? as usize;
        if name_len > MAX_PEER_NAME_LEN {
            return Err(WireError::PeerNameTooLong(name_len));
        }
        let peer_name = std::str::from_utf8(reader.bytes(name_len)?)
            .map_err(|_| WireError::InvalidUtf8)?
            .to_owned();
        let value = Self {
            peer_name,
            lane_index: reader.u16()?,
            lane_count: reader.u16()?,
            max_page: reader.u32()?,
        };
        reader.finish()?;
        value.validate()?;
        Ok(value)
    }

    fn validate(&self) -> Result<(), WireError> {
        let name_len = self.peer_name.len();
        if name_len == 0 {
            return Err(WireError::EmptyPeerName);
        }
        if name_len > MAX_PEER_NAME_LEN {
            return Err(WireError::PeerNameTooLong(name_len));
        }
        if self.lane_count == 0 || self.lane_index >= self.lane_count {
            return Err(WireError::InvalidLane {
                index: self.lane_index,
                count: self.lane_count,
            });
        }
        if self.max_page == 0 || self.max_page as usize > MAX_PAGE_BODY_LEN {
            return Err(WireError::InvalidMaxPage(self.max_page));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RequestMetadata {
    pub stripe: [u8; 32],
    pub src_offset: u64,
    pub src_len: u64,
    pub ttl: u8,
    pub destination_page_count: u32,
}

impl RequestMetadata {
    pub fn encode_metadata(self) -> Result<[u8; REQUEST_METADATA_LEN], WireError> {
        self.validate()?;
        let mut out = [0; REQUEST_METADATA_LEN];
        out[0..32].copy_from_slice(&self.stripe);
        out[32..40].copy_from_slice(&self.src_offset.to_be_bytes());
        out[40..48].copy_from_slice(&self.src_len.to_be_bytes());
        out[48] = self.ttl;
        out[52..56].copy_from_slice(&self.destination_page_count.to_be_bytes());
        Ok(out)
    }

    pub fn decode_metadata(input: &[u8]) -> Result<Self, WireError> {
        if input.len() != REQUEST_METADATA_LEN {
            return Err(WireError::InvalidMetadataLength {
                kind: FrameKind::Request,
                actual: input.len(),
            });
        }
        if input[49..52] != [0; 3] {
            return Err(WireError::NonZeroReserved);
        }
        let value = Self {
            stripe: input[0..32].try_into().unwrap(),
            src_offset: u64::from_be_bytes(input[32..40].try_into().unwrap()),
            src_len: u64::from_be_bytes(input[40..48].try_into().unwrap()),
            ttl: input[48],
            destination_page_count: u32::from_be_bytes(input[52..56].try_into().unwrap()),
        };
        value.validate()?;
        Ok(value)
    }

    fn validate(self) -> Result<(), WireError> {
        if self.src_len == 0 || self.src_offset.checked_add(self.src_len).is_none() {
            return Err(WireError::InvalidSourceRange {
                offset: self.src_offset,
                len: self.src_len,
            });
        }
        if self.destination_page_count == 0
            || self.destination_page_count > MAX_DESTINATION_PAGE_COUNT
        {
            return Err(WireError::InvalidDestinationPageCount(
                self.destination_page_count,
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PageMetadata {
    pub ordinal: u32,
    pub page_offset: u64,
}

impl PageMetadata {
    pub fn encode_metadata(self) -> [u8; PAGE_METADATA_LEN] {
        let mut out = [0; PAGE_METADATA_LEN];
        out[0..4].copy_from_slice(&self.ordinal.to_be_bytes());
        out[4..12].copy_from_slice(&self.page_offset.to_be_bytes());
        out
    }

    pub fn decode_metadata(input: &[u8]) -> Result<Self, WireError> {
        if input.len() != PAGE_METADATA_LEN {
            return Err(WireError::InvalidMetadataLength {
                kind: FrameKind::Page,
                actual: input.len(),
            });
        }
        Ok(Self {
            ordinal: u32::from_be_bytes(input[0..4].try_into().unwrap()),
            page_offset: u64::from_be_bytes(input[4..12].try_into().unwrap()),
        })
    }

    fn validate_body(self, body_len: usize) -> Result<(), WireError> {
        let body_len = u64::try_from(body_len).map_err(|_| WireError::PageRangeOverflow)?;
        self.page_offset
            .checked_add(body_len)
            .ok_or(WireError::PageRangeOverflow)?;
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ErrorMetadata {
    pub code: u32,
    pub message: String,
}

impl ErrorMetadata {
    pub fn encode_metadata(&self) -> Result<Vec<u8>, WireError> {
        self.validate()?;
        let message = self.message.as_bytes();
        let mut out = Vec::with_capacity(ERROR_FIXED_LEN + message.len());
        put_u32(&mut out, self.code);
        put_u16(&mut out, message.len() as u16);
        out.extend_from_slice(message);
        Ok(out)
    }

    pub fn decode_metadata(input: &[u8]) -> Result<Self, WireError> {
        let mut reader = Reader::new(input);
        let code = reader.u32()?;
        let message_len = reader.u16()? as usize;
        if message_len > MAX_ERROR_MESSAGE_LEN {
            return Err(WireError::ErrorMessageTooLong(message_len));
        }
        let message = std::str::from_utf8(reader.bytes(message_len)?)
            .map_err(|_| WireError::InvalidUtf8)?
            .to_owned();
        reader.finish()?;
        let value = Self { code, message };
        value.validate()?;
        Ok(value)
    }

    fn validate(&self) -> Result<(), WireError> {
        if self.code == 0 {
            return Err(WireError::InvalidErrorCode);
        }
        if self.message.len() > MAX_ERROR_MESSAGE_LEN {
            return Err(WireError::ErrorMessageTooLong(self.message.len()));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct FramePrefix<'a> {
    pub header: FrameHeader,
    pub metadata: &'a [u8],
}

impl FramePrefix<'_> {
    pub fn decode_metadata(&self) -> Result<DecodedMetadata, WireError> {
        match self.header.kind {
            FrameKind::Handshake => Ok(DecodedMetadata::Handshake(Handshake::decode_metadata(
                self.metadata,
            )?)),
            FrameKind::Request => Ok(DecodedMetadata::Request(RequestMetadata::decode_metadata(
                self.metadata,
            )?)),
            FrameKind::Page => Ok(DecodedMetadata::Page(PageMetadata::decode_metadata(
                self.metadata,
            )?)),
            FrameKind::Error => Ok(DecodedMetadata::Error(ErrorMetadata::decode_metadata(
                self.metadata,
            )?)),
            FrameKind::End | FrameKind::Cancel => Ok(DecodedMetadata::Empty),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum DecodedMetadata {
    Handshake(Handshake),
    Request(RequestMetadata),
    Page(PageMetadata),
    Error(ErrorMetadata),
    Empty,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Frame<'a> {
    pub header: FrameHeader,
    pub metadata: &'a [u8],
    pub payload: &'a [u8],
}

impl<'a> Frame<'a> {
    pub fn decode_message(&self) -> Result<DecodedMessage<'a>, WireError> {
        match self.header.kind {
            FrameKind::Handshake => Ok(DecodedMessage::Handshake(Handshake::decode_metadata(
                self.metadata,
            )?)),
            FrameKind::Request => Ok(DecodedMessage::Request {
                metadata: RequestMetadata::decode_metadata(self.metadata)?,
                request: self.payload,
            }),
            FrameKind::Page => {
                let metadata = PageMetadata::decode_metadata(self.metadata)?;
                metadata.validate_body(self.payload.len())?;
                Ok(DecodedMessage::Page {
                    metadata,
                    body: self.payload,
                })
            }
            FrameKind::End => Ok(DecodedMessage::End),
            FrameKind::Error => Ok(DecodedMessage::Error(ErrorMetadata::decode_metadata(
                self.metadata,
            )?)),
            FrameKind::Cancel => Ok(DecodedMessage::Cancel),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum DecodedMessage<'a> {
    Handshake(Handshake),
    Request {
        metadata: RequestMetadata,
        request: &'a [u8],
    },
    Page {
        metadata: PageMetadata,
        body: &'a [u8],
    },
    End,
    Error(ErrorMetadata),
    Cancel,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DecodeStatus<T> {
    Complete { value: T, consumed: usize },
    Incomplete { needed: usize },
}

pub fn decode_prefix(input: &[u8]) -> Result<DecodeStatus<FramePrefix<'_>>, WireError> {
    let header = match FrameHeader::decode(input)? {
        DecodeStatus::Complete { value, .. } => value,
        DecodeStatus::Incomplete { needed } => return Ok(DecodeStatus::Incomplete { needed }),
    };
    let prefix_len = header.prefix_len()?;
    if input.len() < prefix_len {
        return Ok(DecodeStatus::Incomplete {
            needed: prefix_len - input.len(),
        });
    }
    Ok(DecodeStatus::Complete {
        value: FramePrefix {
            header,
            metadata: &input[HEADER_LEN..prefix_len],
        },
        consumed: prefix_len,
    })
}

pub fn decode_frame(input: &[u8]) -> Result<DecodeStatus<Frame<'_>>, WireError> {
    let prefix = match decode_prefix(input)? {
        DecodeStatus::Complete { value, .. } => value,
        DecodeStatus::Incomplete { needed } => return Ok(DecodeStatus::Incomplete { needed }),
    };
    let frame_len = prefix.header.frame_len()?;
    if input.len() < frame_len {
        return Ok(DecodeStatus::Incomplete {
            needed: frame_len - input.len(),
        });
    }
    Ok(DecodeStatus::Complete {
        value: Frame {
            header: prefix.header,
            metadata: prefix.metadata,
            payload: &input[prefix.header.prefix_len()?..frame_len],
        },
        consumed: frame_len,
    })
}

pub fn encode_frame(
    kind: FrameKind,
    request_id: u64,
    metadata: &[u8],
    payload: &[u8],
) -> Result<Vec<u8>, WireError> {
    let header = FrameHeader::new(kind, request_id, metadata.len(), payload.len())?;
    let frame_len = header.frame_len()?;
    let mut out = Vec::with_capacity(frame_len);
    out.extend_from_slice(&header.encode()?);
    out.extend_from_slice(metadata);
    out.extend_from_slice(payload);
    Ok(out)
}

pub fn encode_handshake(handshake: &Handshake) -> Result<Vec<u8>, WireError> {
    encode_frame(FrameKind::Handshake, 0, &handshake.encode_metadata()?, &[])
}

pub fn encode_request(
    request_id: u64,
    metadata: RequestMetadata,
    request: &[u8],
) -> Result<Vec<u8>, WireError> {
    encode_frame(
        FrameKind::Request,
        request_id,
        &metadata.encode_metadata()?,
        request,
    )
}

/// Encodes only the header and page metadata. The body remains separate so a
/// receiver can submit it directly to `recv_fixed` after parsing this prefix.
pub fn encode_page_prefix(
    request_id: u64,
    metadata: PageMetadata,
    body_len: usize,
) -> Result<Vec<u8>, WireError> {
    metadata.validate_body(body_len)?;
    let metadata = metadata.encode_metadata();
    let header = FrameHeader::new(FrameKind::Page, request_id, metadata.len(), body_len)?;
    let mut out = Vec::with_capacity(header.prefix_len()?);
    out.extend_from_slice(&header.encode()?);
    out.extend_from_slice(&metadata);
    Ok(out)
}

pub fn encode_end(request_id: u64) -> Result<Vec<u8>, WireError> {
    encode_frame(FrameKind::End, request_id, &[], &[])
}

pub fn encode_error(request_id: u64, error: &ErrorMetadata) -> Result<Vec<u8>, WireError> {
    encode_frame(FrameKind::Error, request_id, &error.encode_metadata()?, &[])
}

pub fn encode_cancel(request_id: u64) -> Result<Vec<u8>, WireError> {
    encode_frame(FrameKind::Cancel, request_id, &[], &[])
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum WireError {
    InvalidMagic,
    UnsupportedVersion(u8),
    UnknownKind(u8),
    UnsupportedFlags(u16),
    MetadataTooLarge(usize),
    PayloadTooLarge(usize),
    InvalidMetadataLength { kind: FrameKind, actual: usize },
    InvalidPayloadLength { kind: FrameKind, actual: usize },
    InvalidRequestId { kind: FrameKind, request_id: u64 },
    FrameLengthOverflow,
    TruncatedMetadata,
    TrailingMetadata,
    InvalidUtf8,
    EmptyPeerName,
    PeerNameTooLong(usize),
    InvalidLane { index: u16, count: u16 },
    InvalidMaxPage(u32),
    NonZeroReserved,
    InvalidSourceRange { offset: u64, len: u64 },
    InvalidDestinationPageCount(u32),
    PageRangeOverflow,
    InvalidErrorCode,
    ErrorMessageTooLong(usize),
}

impl fmt::Display for WireError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "invalid TCP RPC wire data: {self:?}")
    }
}

impl Error for WireError {}

struct Reader<'a> {
    input: &'a [u8],
    offset: usize,
}

impl<'a> Reader<'a> {
    fn new(input: &'a [u8]) -> Self {
        Self { input, offset: 0 }
    }

    fn bytes(&mut self, len: usize) -> Result<&'a [u8], WireError> {
        let end = self
            .offset
            .checked_add(len)
            .ok_or(WireError::FrameLengthOverflow)?;
        let bytes = self
            .input
            .get(self.offset..end)
            .ok_or(WireError::TruncatedMetadata)?;
        self.offset = end;
        Ok(bytes)
    }

    fn u16(&mut self) -> Result<u16, WireError> {
        Ok(u16::from_be_bytes(self.bytes(2)?.try_into().unwrap()))
    }

    fn u32(&mut self) -> Result<u32, WireError> {
        Ok(u32::from_be_bytes(self.bytes(4)?.try_into().unwrap()))
    }

    fn finish(self) -> Result<(), WireError> {
        if self.offset == self.input.len() {
            Ok(())
        } else {
            Err(WireError::TrailingMetadata)
        }
    }
}

fn put_u16(out: &mut Vec<u8>, value: u16) {
    out.extend_from_slice(&value.to_be_bytes());
}

fn put_u32(out: &mut Vec<u8>, value: u32) {
    out.extend_from_slice(&value.to_be_bytes());
}

fn checked_add(left: usize, right: usize) -> Result<usize, WireError> {
    left.checked_add(right)
        .ok_or(WireError::FrameLengthOverflow)
}

fn require_request_id(
    kind: FrameKind,
    request_id: u64,
    must_be_zero: bool,
) -> Result<(), WireError> {
    if (must_be_zero && request_id != 0) || (!must_be_zero && request_id == 0) {
        Err(WireError::InvalidRequestId { kind, request_id })
    } else {
        Ok(())
    }
}

fn require_metadata_len(kind: FrameKind, actual: usize, expected: usize) -> Result<(), WireError> {
    if actual == expected {
        Ok(())
    } else {
        Err(WireError::InvalidMetadataLength { kind, actual })
    }
}

fn require_metadata_range(
    kind: FrameKind,
    actual: usize,
    min: usize,
    max: usize,
) -> Result<(), WireError> {
    if (min..=max).contains(&actual) {
        Ok(())
    } else {
        Err(WireError::InvalidMetadataLength { kind, actual })
    }
}

fn require_payload_len(kind: FrameKind, actual: usize, expected: usize) -> Result<(), WireError> {
    if actual == expected {
        Ok(())
    } else {
        Err(WireError::InvalidPayloadLength { kind, actual })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn handshake() -> Handshake {
        Handshake {
            peer_name: "peer-a".to_owned(),
            lane_index: 1,
            lane_count: 3,
            max_page: 2 * 1024 * 1024,
        }
    }

    fn request_metadata() -> RequestMetadata {
        RequestMetadata {
            stripe: [0x5a; 32],
            src_offset: 0x0102_0304_0506_0708,
            src_len: 8192,
            ttl: 7,
            destination_page_count: 4,
        }
    }

    fn complete_frame(input: &[u8]) -> Frame<'_> {
        match decode_frame(input).expect("decode frame") {
            DecodeStatus::Complete { value, consumed } => {
                assert_eq!(consumed, input.len());
                value
            }
            DecodeStatus::Incomplete { needed } => panic!("missing {needed} bytes"),
        }
    }

    #[test]
    fn header_uses_fixed_network_byte_order_layout() {
        let header = FrameHeader::new(FrameKind::Request, 0x0102_0304_0506_0708, 56, 9)
            .unwrap()
            .encode()
            .unwrap();
        assert_eq!(&header[0..4], b"UBRP");
        assert_eq!(&header[4..8], &[1, 2, 0, 0]);
        assert_eq!(&header[8..12], &[0, 0, 0, 56]);
        assert_eq!(&header[12..16], &[0, 0, 0, 9]);
        assert_eq!(&header[16..24], &[1, 2, 3, 4, 5, 6, 7, 8]);
    }

    #[test]
    fn typed_messages_round_trip() {
        let encoded = encode_handshake(&handshake()).unwrap();
        assert_eq!(
            complete_frame(&encoded).decode_message().unwrap(),
            DecodedMessage::Handshake(handshake())
        );

        let request = b"opaque request";
        let encoded = encode_request(9, request_metadata(), request).unwrap();
        assert_eq!(
            complete_frame(&encoded).decode_message().unwrap(),
            DecodedMessage::Request {
                metadata: request_metadata(),
                request,
            }
        );

        let error = ErrorMetadata {
            code: 503,
            message: "busy".to_owned(),
        };
        let encoded = encode_error(9, &error).unwrap();
        assert_eq!(
            complete_frame(&encoded).decode_message().unwrap(),
            DecodedMessage::Error(error)
        );
        assert!(matches!(
            complete_frame(&encode_end(9).unwrap())
                .decode_message()
                .unwrap(),
            DecodedMessage::End
        ));
        assert!(matches!(
            complete_frame(&encode_cancel(9).unwrap())
                .decode_message()
                .unwrap(),
            DecodedMessage::Cancel
        ));
    }

    #[test]
    fn page_prefix_keeps_body_separate() {
        let metadata = PageMetadata {
            ordinal: 3,
            page_offset: 4096,
        };
        let prefix_bytes = encode_page_prefix(42, metadata, 8192).unwrap();
        assert_eq!(prefix_bytes.len(), HEADER_LEN + PAGE_METADATA_LEN);
        let prefix = match decode_prefix(&prefix_bytes).unwrap() {
            DecodeStatus::Complete { value, consumed } => {
                assert_eq!(consumed, prefix_bytes.len());
                value
            }
            DecodeStatus::Incomplete { .. } => panic!("complete prefix"),
        };
        assert_eq!(prefix.header.payload_len, 8192);
        assert_eq!(
            prefix.decode_metadata().unwrap(),
            DecodedMetadata::Page(metadata)
        );
        assert_eq!(
            decode_frame(&prefix_bytes).unwrap(),
            DecodeStatus::Incomplete { needed: 8192 }
        );
    }

    #[test]
    fn decoder_reports_every_fragment_boundary() {
        let encoded = encode_request(11, request_metadata(), b"abcdef").unwrap();
        for available in 0..encoded.len() {
            let status = decode_frame(&encoded[..available]).unwrap();
            let expected_needed = if available < HEADER_LEN {
                HEADER_LEN - available
            } else if available < HEADER_LEN + REQUEST_METADATA_LEN {
                HEADER_LEN + REQUEST_METADATA_LEN - available
            } else {
                encoded.len() - available
            };
            assert_eq!(
                status,
                DecodeStatus::Incomplete {
                    needed: expected_needed,
                },
                "fragment boundary {available}"
            );
        }
        assert!(matches!(
            decode_frame(&encoded).unwrap(),
            DecodeStatus::Complete { .. }
        ));
    }

    #[test]
    fn decoder_leaves_coalesced_frame_for_the_caller() {
        let first = encode_end(1).unwrap();
        let second = encode_cancel(2).unwrap();
        let bytes = [first.as_slice(), second.as_slice()].concat();

        let consumed = match decode_frame(&bytes).unwrap() {
            DecodeStatus::Complete { value, consumed } => {
                assert_eq!(value.header.kind, FrameKind::End);
                consumed
            }
            DecodeStatus::Incomplete { .. } => panic!("first frame is complete"),
        };
        assert_eq!(consumed, first.len());
        assert_eq!(
            complete_frame(&bytes[consumed..]).header.kind,
            FrameKind::Cancel
        );
    }

    #[test]
    fn invalid_header_fields_are_rejected() {
        let valid = encode_end(1).unwrap();
        for (index, value, expected) in [
            (0, b'X', WireError::InvalidMagic),
            (4, VERSION + 1, WireError::UnsupportedVersion(VERSION + 1)),
            (5, 99, WireError::UnknownKind(99)),
        ] {
            let mut bytes = valid.clone();
            bytes[index] = value;
            assert_eq!(decode_frame(&bytes).unwrap_err(), expected);
        }

        let mut bytes = valid;
        bytes[7] = 1;
        assert_eq!(
            decode_frame(&bytes).unwrap_err(),
            WireError::UnsupportedFlags(1)
        );
    }

    #[test]
    fn invalid_lengths_and_request_ids_are_rejected() {
        let mut bytes = encode_end(1).unwrap();
        bytes[11] = 1;
        assert_eq!(
            decode_frame(&bytes).unwrap_err(),
            WireError::InvalidMetadataLength {
                kind: FrameKind::End,
                actual: 1,
            }
        );

        let mut bytes = encode_end(1).unwrap();
        bytes[15] = 1;
        assert_eq!(
            decode_frame(&bytes).unwrap_err(),
            WireError::InvalidPayloadLength {
                kind: FrameKind::End,
                actual: 1,
            }
        );

        let mut bytes = encode_end(1).unwrap();
        bytes[16..24].fill(0);
        assert_eq!(
            decode_frame(&bytes).unwrap_err(),
            WireError::InvalidRequestId {
                kind: FrameKind::End,
                request_id: 0,
            }
        );

        assert!(matches!(
            FrameHeader::new(
                FrameKind::Request,
                1,
                REQUEST_METADATA_LEN,
                MAX_REQUEST_BYTES + 1
            ),
            Err(WireError::PayloadTooLarge(_))
        ));
        assert!(matches!(
            FrameHeader::new(FrameKind::Page, 1, PAGE_METADATA_LEN, MAX_PAGE_BODY_LEN + 1),
            Err(WireError::InvalidPayloadLength { .. })
        ));
        assert!(matches!(
            FrameHeader::new(FrameKind::Error, 1, MAX_METADATA_LEN + 1, 0),
            Err(WireError::MetadataTooLarge(_))
        ));
    }

    #[test]
    fn metadata_utf8_and_exact_length_are_enforced() {
        let mut metadata = handshake().encode_metadata().unwrap();
        metadata[2] = 0xff;
        assert_eq!(
            Handshake::decode_metadata(&metadata).unwrap_err(),
            WireError::InvalidUtf8
        );

        let mut metadata = ErrorMetadata {
            code: 1,
            message: "x".to_owned(),
        }
        .encode_metadata()
        .unwrap();
        metadata.push(0);
        assert_eq!(
            ErrorMetadata::decode_metadata(&metadata).unwrap_err(),
            WireError::TrailingMetadata
        );

        let mut metadata = request_metadata().encode_metadata().unwrap();
        metadata[50] = 1;
        assert_eq!(
            RequestMetadata::decode_metadata(&metadata).unwrap_err(),
            WireError::NonZeroReserved
        );
    }

    #[test]
    fn semantic_and_overflow_validation_is_strict() {
        let mut value = handshake();
        value.lane_index = value.lane_count;
        assert!(matches!(
            value.encode_metadata(),
            Err(WireError::InvalidLane { .. })
        ));

        let mut request = request_metadata();
        request.src_offset = u64::MAX;
        request.src_len = 2;
        assert!(matches!(
            request.encode_metadata(),
            Err(WireError::InvalidSourceRange { .. })
        ));

        let mut request = request_metadata();
        request.destination_page_count = 0;
        assert_eq!(
            request.encode_metadata().unwrap_err(),
            WireError::InvalidDestinationPageCount(0)
        );

        assert_eq!(
            encode_page_prefix(
                1,
                PageMetadata {
                    ordinal: 0,
                    page_offset: u64::MAX,
                },
                1,
            )
            .unwrap_err(),
            WireError::PageRangeOverflow
        );
        assert_eq!(
            ErrorMetadata {
                code: 0,
                message: String::new(),
            }
            .encode_metadata()
            .unwrap_err(),
            WireError::InvalidErrorCode
        );
        assert_eq!(
            checked_add(usize::MAX, 1).unwrap_err(),
            WireError::FrameLengthOverflow
        );
    }

    #[test]
    fn duplicate_bytes_cannot_hide_in_typed_metadata() {
        let mut metadata = handshake().encode_metadata().unwrap();
        metadata.extend_from_slice(&handshake().encode_metadata().unwrap());
        assert_eq!(
            Handshake::decode_metadata(&metadata).unwrap_err(),
            WireError::TrailingMetadata
        );
    }
}
