// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Record-aware receive helpers shared by the HTTP-family origin
//! backends (`http`, `s3`, `azure`).
//!
//! On a plaintext connection these are thin pass-throughs to the
//! ring's `recv` / `recv_fixed`. On a kernel-TLS connection the kernel
//! decrypts in place, but a plain `recv` returns `EIO` whenever a
//! non-application-data TLS record (a post-handshake ticket, a key
//! update, or a close-notify alert) sits at the head of the stream.
//! These helpers therefore switch to the record-typed `recv_msg` /
//! `recv_fixed_msg` ops and apply a uniform record policy:
//!
//! - `application_data` (23): deliver the bytes (zero copy preserved
//!   for the fixed/body path; the kernel wrote plaintext straight into
//!   the page).
//! - `handshake` (22): a post-handshake message (e.g. NewSessionTicket
//!   or KeyUpdate). Discard whatever landed and receive again at the
//!   same position.
//! - `alert` (21): a `close_notify` is a clean end of stream; any other
//!   alert is fatal and aborted the transfer, so it surfaces as an
//!   error rather than a silent end of stream.
//!
//! A zero-length read is also end of stream. To avoid spinning forever
//! on an origin that streams control records without ever delivering
//! application data, a bounded number of consecutive non-data records
//! is tolerated before the read fails.

use std::os::fd::RawFd;

use crate::bufferpool::Error;
use crate::ring::{
    NetHandle, TLS_RECORD_TYPE_ALERT, TLS_RECORD_TYPE_APPLICATION_DATA, TLS_RECORD_TYPE_HANDSHAKE,
};

/// Chunk size for header-accumulation reads (the heap path).
pub(super) const RECV_CHUNK: usize = 64 * 1024;

/// TLS `close_notify` alert description (RFC 8446 6.1). It is the only
/// alert that signals a graceful end of stream; every other alert is a
/// failure that truncated the transfer.
const TLS_ALERT_CLOSE_NOTIFY: u8 = 0;

/// Upper bound on consecutive non-application-data records tolerated in
/// a single logical read before giving up. Legitimate post-handshake
/// traffic is a handful of records (a NewSessionTicket, the occasional
/// KeyUpdate); an origin that streams control records without ever
/// delivering application data is broken or hostile, so refuse to spin
/// on it indefinitely.
const MAX_CONTROL_RECORDS: u32 = 64;

/// Map an `alert` record to an end-of-stream result. A `close_notify`
/// is a graceful close (empty read); any other alert truncated the
/// stream and is reported as an error. `desc` is the alert description
/// byte, or `None` when the alert payload was too short to classify (in
/// which case it is treated leniently as a graceful close).
fn alert_eos<T: Default>(desc: Option<u8>) -> Result<T, Error> {
    match desc {
        Some(d) if d != TLS_ALERT_CLOSE_NOTIFY => Err(Error::transport(std::io::Error::new(
            std::io::ErrorKind::ConnectionReset,
            format!("tls: fatal alert {d} before end of response"),
        ))),
        _ => Ok(T::default()),
    }
}

/// Error for an origin that exceeded [`MAX_CONTROL_RECORDS`] consecutive
/// non-application-data records.
fn too_many_control_records<T>() -> Result<T, Error> {
    Err(Error::transport(std::io::Error::new(
        std::io::ErrorKind::InvalidData,
        "tls: too many consecutive non-application-data records",
    )))
}

/// Receive one chunk of response bytes into a fresh heap buffer.
///
/// When `tls` is false this is the original plaintext `recv`. When
/// true it loops over record-typed `recv_msg` reads, skipping
/// post-handshake records and mapping a `close_notify` alert to a
/// zero-length (end of stream) result while rejecting fatal alerts.
pub(super) async fn recv_chunk(
    handle: &NetHandle,
    fd: RawFd,
    tls: bool,
) -> Result<Vec<u8>, Error> {
    if !tls {
        return handle.recv(fd, RECV_CHUNK).await.map_err(io_to_err);
    }

    let mut control_records: u32 = 0;
    loop {
        let (data, record_type) = handle.recv_msg(fd, RECV_CHUNK).await.map_err(io_to_err)?;
        if data.is_empty() {
            return Ok(Vec::new());
        }
        match record_type {
            TLS_RECORD_TYPE_APPLICATION_DATA => return Ok(data),
            // The alert payload is `[level, description]`; classify on
            // the description byte.
            TLS_RECORD_TYPE_ALERT => return alert_eos(data.get(1).copied()),
            // Handshake and any other non-data record: discard and read
            // again, but do not feed control bytes to the HTTP parser.
            TLS_RECORD_TYPE_HANDSHAKE => {}
            _ => {}
        }
        control_records += 1;
        if control_records > MAX_CONTROL_RECORDS {
            return too_many_control_records();
        }
    }
}

/// Receive response-body bytes directly into a registered backing page
/// at `page_byte_offset`, returning the number of plaintext bytes
/// written. Zero copy: the kernel decrypts (when TLS) straight into the
/// page.
///
/// When `tls` is false this is the original plaintext `recv_fixed`.
/// When true it loops over record-typed `recv_fixed_msg` reads with the
/// same record policy as [`recv_chunk`]; a handshake record overwrites
/// the same page bytes on retry (they are discarded, not delivered).
pub(super) async fn recv_fixed(
    handle: &NetHandle,
    fd: RawFd,
    tls: bool,
    page_byte_offset: usize,
    len: usize,
) -> Result<usize, Error> {
    if !tls {
        return handle
            .recv_fixed(fd, 0, page_byte_offset, len)
            .await
            .map_err(io_to_err);
    }

    let mut control_records: u32 = 0;
    loop {
        let record = handle
            .recv_fixed_msg(fd, page_byte_offset, len)
            .await
            .map_err(io_to_err)?;
        if record.len == 0 {
            return Ok(0);
        }
        match record.record_type {
            TLS_RECORD_TYPE_APPLICATION_DATA => return Ok(record.len),
            TLS_RECORD_TYPE_ALERT => return alert_eos(record.alert_desc),
            TLS_RECORD_TYPE_HANDSHAKE => {}
            _ => {}
        }
        control_records += 1;
        if control_records > MAX_CONTROL_RECORDS {
            return too_many_control_records();
        }
    }
}

fn io_to_err(e: std::io::Error) -> Error {
    match e.raw_os_error() {
        Some(code) => Error::Io(code),
        None => Error::transport(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn alert_eos_close_notify_is_graceful_end_of_stream() {
        // close_notify (description 0) ends the stream cleanly.
        let v: Vec<u8> = alert_eos(Some(TLS_ALERT_CLOSE_NOTIFY)).expect("close_notify is graceful");
        assert!(v.is_empty());
        let n: usize = alert_eos(Some(TLS_ALERT_CLOSE_NOTIFY)).expect("close_notify is graceful");
        assert_eq!(n, 0);
    }

    #[test]
    fn alert_eos_fatal_alert_is_error() {
        // A non-close_notify alert truncated the transfer and must error
        // rather than masquerade as a clean end of stream. 20 ==
        // bad_record_mac, 51 == decrypt_error: both fatal.
        for desc in [20u8, 51u8] {
            let r: Result<Vec<u8>, _> = alert_eos(Some(desc));
            assert!(r.is_err(), "fatal alert {desc} must be an error");
            let r: Result<usize, _> = alert_eos(Some(desc));
            assert!(r.is_err(), "fatal alert {desc} must be an error");
        }
    }

    #[test]
    fn alert_eos_unclassifiable_alert_is_lenient() {
        // A sub-2-byte alert payload cannot be classified; treat it as a
        // graceful close rather than fabricating a failure.
        let v: Vec<u8> = alert_eos(None).expect("unknown alert tolerated");
        assert!(v.is_empty());
    }
}
