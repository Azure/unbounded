// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Fixed peer-failure codec shared by authenticated HTTP and RDMA adapters.
use super::{Cause, PeerEvidence, PeerFailure, PeerReason, Phase, Transport};
use std::{io, net::SocketAddr};

impl PeerFailure {
    pub const LEN: usize = 61;
    pub fn encode(self) -> [u8; Self::LEN] {
        let mut out = [0; Self::LEN];
        out[..32].copy_from_slice(&self.identity);
        out[32..36].copy_from_slice(&self.candidate.to_be_bytes());
        out[36] = self.reason as u8;
        if let Some(e) = self.evidence {
            out[37] = match e.endpoint.ip() {
                std::net::IpAddr::V4(ip) => {
                    out[38..42].copy_from_slice(&ip.octets());
                    4
                }
                std::net::IpAddr::V6(ip) => {
                    out[38..54].copy_from_slice(&ip.octets());
                    6
                }
            };
            out[54..56].copy_from_slice(&e.endpoint.port().to_be_bytes());
            out[56] = match e.transport {
                Transport::Http => 1,
                Transport::Rdma => 2,
            };
            out[57] = match e.phase {
                Phase::LocalAdmission => 1,
                Phase::Connect => 2,
                Phase::Send => 3,
                Phase::Headers => 4,
                Phase::Body => 5,
                Phase::Grant => 6,
                Phase::Read => 7,
            };
            out[58] = match e.cause {
                Cause::Connection => 1,
                Cause::ServiceTimeout => 2,
                Cause::CallerDeadline => 3,
                Cause::LocalPressure => 4,
                Cause::Cancelled => 5,
                Cause::Protocol => 6,
                Cause::BreakerRejected => 7,
                Cause::Other => 8,
            };
            out[59] = u8::from(e.initiated);
        }
        out
    }
    pub fn decode(bytes: &[u8]) -> io::Result<Self> {
        if bytes.len() != Self::LEN {
            return Err(io::ErrorKind::InvalidData.into());
        }
        let reason = match bytes[36] {
            1 => PeerReason::OwnerUnavailable,
            2 => PeerReason::Busy,
            3 => PeerReason::Unavailable,
            4 => PeerReason::Protocol,
            5 => PeerReason::Service,
            6 => PeerReason::Deadline,
            7 => PeerReason::Cancelled,
            8 => PeerReason::NotFound,
            9 => PeerReason::Gone,
            10 => PeerReason::Precondition,
            _ => return Err(io::ErrorKind::InvalidData.into()),
        };
        let bad = || io::Error::from(io::ErrorKind::InvalidData);
        let evidence = if bytes[37] == 0 {
            if bytes[38..].iter().any(|b| *b != 0) {
                return Err(bad());
            }
            None
        } else {
            let ip = match bytes[37] {
                4 if bytes[42..54].iter().all(|b| *b == 0) => std::net::IpAddr::V4(
                    std::net::Ipv4Addr::from(<[u8; 4]>::try_from(&bytes[38..42]).unwrap()),
                ),
                6 => std::net::IpAddr::V6(std::net::Ipv6Addr::from(
                    <[u8; 16]>::try_from(&bytes[38..54]).unwrap(),
                )),
                _ => return Err(bad()),
            };
            if bytes[59] > 1 || bytes[60] != 0 {
                return Err(bad());
            }
            Some(PeerEvidence {
                endpoint: SocketAddr::new(
                    ip,
                    u16::from_be_bytes(bytes[54..56].try_into().unwrap()),
                ),
                transport: match bytes[56] {
                    1 => Transport::Http,
                    2 => Transport::Rdma,
                    _ => return Err(bad()),
                },
                phase: match bytes[57] {
                    1 => Phase::LocalAdmission,
                    2 => Phase::Connect,
                    3 => Phase::Send,
                    4 => Phase::Headers,
                    5 => Phase::Body,
                    6 => Phase::Grant,
                    7 => Phase::Read,
                    _ => return Err(bad()),
                },
                cause: match bytes[58] {
                    1 => Cause::Connection,
                    2 => Cause::ServiceTimeout,
                    3 => Cause::CallerDeadline,
                    4 => Cause::LocalPressure,
                    5 => Cause::Cancelled,
                    6 => Cause::Protocol,
                    7 => Cause::BreakerRejected,
                    8 => Cause::Other,
                    _ => return Err(bad()),
                },
                initiated: bytes[59] == 1,
            })
        };
        Ok(Self {
            identity: bytes[..32].try_into().unwrap(),
            candidate: u32::from_be_bytes(bytes[32..36].try_into().unwrap()),
            reason,
            evidence,
        })
    }
}
