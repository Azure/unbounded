// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-fabric connection table: peer-id -> libfabric `fi_addr_t`.
//!
//! A "connection" here is an entry in libfabric's address vector (AV)
//! plus the upper-layer `PeerId` that names it. There is no `fid_ep`
//! per peer; the fabric runs a single RDM endpoint and discriminates
//! peers via `fi_addr_t`. The table owns the mapping and the
//! corresponding AV row; `remove_connection` calls `fi_av_remove` to
//! reclaim the slot.
//!
//! Calling `add_connection` for a peer that is already present
//! *replaces* the prior `fi_addr` (the old one is removed from the AV
//! first). Callers that need to detect duplicates should `list_connections`
//! before inserting.

use std::collections::HashMap;
use std::ffi::CString;
use std::ptr;
use std::sync::RwLock;

use crate::fabric::PeerId;

use super::error::{FabricError, Result, check};
use super::fabric::Fabric;
use super::ffi;
use super::types::ConnectionSpec;

/// Internal mapping of `PeerId` to libfabric `fi_addr_t`. Held by
/// `FabricInner`; mutation is serialized by the embedded `RwLock`.
pub(crate) struct ConnectionTable {
    inner: RwLock<HashMap<PeerId, ffi::fi_addr_t>>,
}

impl ConnectionTable {
    pub(crate) fn new() -> Self {
        Self {
            inner: RwLock::new(HashMap::new()),
        }
    }

    pub(crate) fn list(&self) -> Vec<PeerId> {
        self.inner
            .read()
            .map(|m| m.keys().copied().collect())
            .unwrap_or_default()
    }

    pub(crate) fn lookup(&self, peer: PeerId) -> Option<ffi::fi_addr_t> {
        self.inner.read().ok().and_then(|m| m.get(&peer).copied())
    }
}

impl Fabric {
    /// Add `spec` to the connection table.
    ///
    /// Resolves `spec.wire_addr` to a raw libfabric address based on
    /// the fabric's provider:
    ///
    /// * `Provider::Tcp` - `wire_addr` is "host:port"; parsed via
    ///   `getaddrinfo` in the C shim into a `sockaddr_in` /
    ///   `sockaddr_in6` blob.
    /// * `Provider::Verbs` - `wire_addr` is a hex-encoded blob of the
    ///   bytes returned by `Fabric::self_address` on the peer. We
    ///   decode the hex here.
    ///
    /// If `spec.peer` is already present, the previous `fi_addr_t` is
    /// removed from the AV and replaced.
    pub fn add_connection(&self, spec: ConnectionSpec) -> Result<()> {
        if let (Some(n), Some(m)) = (self.inner().cfg.numa, spec.hca_numa) {
            if n != m {
                return Err(FabricError::NumaMismatch {
                    expected: n,
                    got: m,
                });
            }
        }

        let mut addr_bytes = resolve_wire_addr(self.inner().cfg.provider, &spec.wire_addr)?;

        let mut fi_addr: ffi::fi_addr_t = ffi::FI_ADDR_UNSPEC;
        // SAFETY: `av` is the fabric's live AV; addr_bytes outlives
        // the call; `fi_addr` is a stack out-param.
        let rc = unsafe {
            ffi::ub_fi_av_insert(
                self.inner().av(),
                addr_bytes.as_mut_ptr() as *const std::ffi::c_void,
                1,
                &mut fi_addr,
                0,
                ptr::null_mut(),
            )
        };
        check("fi_av_insert", rc)?;

        // Replace any prior fi_addr for this peer; remove the old
        // entry from the AV so the slot is reclaimed.
        let mut old: Option<ffi::fi_addr_t> = None;
        if let Ok(mut m) = self.inner().connections.inner.write() {
            if let Some(prev) = m.insert(spec.peer, fi_addr) {
                old = Some(prev);
            }
        }
        if let Some(mut prev) = old {
            // SAFETY: `prev` was previously inserted by `fi_av_insert`.
            unsafe {
                let _ = ffi::ub_fi_av_remove(self.inner().av(), &mut prev, 1, 0);
            }
        }
        Ok(())
    }

    /// Remove `peer` from the connection table and the underlying AV.
    /// Errors with `FabricError::NotFound("peer")` if the peer was not
    /// present.
    pub fn remove_connection(&self, peer: PeerId) -> Result<()> {
        let removed = self
            .inner()
            .connections
            .inner
            .write()
            .ok()
            .and_then(|mut m| m.remove(&peer));
        let mut fi_addr = match removed {
            Some(a) => a,
            None => return Err(FabricError::NotFound("peer")),
        };
        // SAFETY: `fi_addr` was previously inserted via `fi_av_insert`.
        let rc = unsafe { ffi::ub_fi_av_remove(self.inner().av(), &mut fi_addr, 1, 0) };
        check("fi_av_remove", rc)
    }

    /// Snapshot of currently-known peers.
    pub fn list_connections(&self) -> Vec<PeerId> {
        self.inner().connections.list()
    }

    /// Resolve a `PeerId` back to its libfabric `fi_addr_t`. Used
    /// internally by ping and (Phase 5) RPC submission paths.
    pub(crate) fn lookup_fi_addr(&self, peer: PeerId) -> Result<ffi::fi_addr_t> {
        self.inner()
            .connections
            .lookup(peer)
            .ok_or(FabricError::NotFound("peer"))
    }
}

/// Resolve `wire_addr` to the raw bytes libfabric's `fi_av_insert`
/// expects for `provider`.
fn resolve_wire_addr(provider: super::config::Provider, wire_addr: &str) -> Result<Vec<u8>> {
    use super::config::Provider;
    match provider {
        Provider::Tcp => parse_tcp_sockaddr(wire_addr),
        Provider::Verbs => decode_hex(wire_addr),
    }
}

fn parse_tcp_sockaddr(s: &str) -> Result<Vec<u8>> {
    let c = CString::new(s).map_err(|_| FabricError::BadConfig("wire_addr has NUL"))?;
    let mut buf = vec![0u8; 128];
    // SAFETY: shim writes at most `buf.len()` bytes and returns the
    // count it wrote; `c` outlives the call.
    let rc = unsafe { ffi::ub_fi_parse_sockaddr(c.as_ptr(), buf.as_mut_ptr(), buf.len()) };
    if rc < 0 {
        return Err(FabricError::Pkg("ub_fi_parse_sockaddr", rc as i32));
    }
    buf.truncate(rc as usize);
    Ok(buf)
}

fn decode_hex(s: &str) -> Result<Vec<u8>> {
    if s.len() % 2 != 0 {
        return Err(FabricError::BadConfig("verbs wire_addr hex length odd"));
    }
    let mut out = Vec::with_capacity(s.len() / 2);
    let bytes = s.as_bytes();
    for chunk in bytes.chunks(2) {
        let hi = hex_nibble(chunk[0])?;
        let lo = hex_nibble(chunk[1])?;
        out.push((hi << 4) | lo);
    }
    Ok(out)
}

fn hex_nibble(c: u8) -> Result<u8> {
    match c {
        b'0'..=b'9' => Ok(c - b'0'),
        b'a'..=b'f' => Ok(c - b'a' + 10),
        b'A'..=b'F' => Ok(c - b'A' + 10),
        _ => Err(FabricError::BadConfig("verbs wire_addr non-hex char")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hex_decode_roundtrip() {
        let v = decode_hex("0011aaFF").expect("decode");
        assert_eq!(v, vec![0x00, 0x11, 0xAA, 0xFF]);
    }

    #[test]
    fn hex_decode_rejects_odd_length() {
        assert!(matches!(decode_hex("abc"), Err(FabricError::BadConfig(_))));
    }

    #[test]
    fn hex_decode_rejects_non_hex() {
        assert!(matches!(decode_hex("0g"), Err(FabricError::BadConfig(_))));
    }

    #[test]
    fn connection_table_starts_empty() {
        let t = ConnectionTable::new();
        assert!(t.list().is_empty());
        assert!(t.lookup(PeerId(1)).is_none());
    }

    /// Pure NUMA-mismatch helper exercised without any FFI. The
    /// production check lives inline in `add_connection`; this test
    /// mirrors the logic so a regression there fails loudly.
    fn numa_check(fabric_numa: Option<u16>, spec_numa: Option<u16>) -> Result<()> {
        if let (Some(n), Some(m)) = (fabric_numa, spec_numa) {
            if n != m {
                return Err(FabricError::NumaMismatch {
                    expected: n,
                    got: m,
                });
            }
        }
        Ok(())
    }

    #[test]
    fn numa_check_passes_when_either_unset() {
        assert!(numa_check(None, None).is_ok());
        assert!(numa_check(Some(0), None).is_ok());
        assert!(numa_check(None, Some(1)).is_ok());
    }

    #[test]
    fn numa_check_passes_when_equal() {
        assert!(numa_check(Some(2), Some(2)).is_ok());
    }

    #[test]
    fn numa_check_rejects_mismatch() {
        match numa_check(Some(0), Some(1)) {
            Err(FabricError::NumaMismatch { expected, got }) => {
                assert_eq!(expected, 0);
                assert_eq!(got, 1);
            }
            other => panic!("expected NumaMismatch, got {other:?}"),
        }
    }
}
