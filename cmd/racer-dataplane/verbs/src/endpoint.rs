//! Fixed-width endpoint bytes. Authentication and envelopes belong to callers.
use crate::{Endpoint, Error, Result};

impl Endpoint {
    pub const ENCODED_LEN: usize = 32;

    /// Encode fields in network byte order, independently of native ABI layout.
    /// As with native endpoint construction, validation is explicit on encode.
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

    /// Decode exactly one endpoint and validate its verbs field bounds.
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

#[cfg(test)]
mod tests {
    use super::*;

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
