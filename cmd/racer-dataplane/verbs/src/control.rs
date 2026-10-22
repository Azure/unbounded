//! Fixed-size in-band control message codec (v1, 64 bytes, network order).
//!
//! See `designs/racer-verbs.md`, "Control messages". Decoding is strict: any
//! malformed message is an error, and the caller breaks the link. Per-link
//! checks that need state (unknown or duplicate tag, credit overrun) belong to
//! the link layer, not here.
//!
//! Layout:
//!
//! | Offset | Size | Field |
//! |---|---|---|
//! | 0 | 1 | version = 1 |
//! | 1 | 1 | kind: 1 Grant, 2 Release, 3 Credit, 4 Close |
//! | 2 | 2 | reserved, zero |
//! | 4 | 4 | credits returned |
//! | 8 | 8 | link id |
//! | 16 | 4 | tag (nonzero for Grant and Release, zero otherwise) |
//! | 20 | 4 | rkey (Grant only, zero otherwise) |
//! | 24 | 8 | remote address (Grant only, zero otherwise) |
//! | 32 | 4 | length (Grant only, 1..=buffer_len; zero otherwise) |
//! | 36 | 4 | reserved, zero |
//! | 40 | 16 | transfer id (Grant and Release, zero otherwise) |
//! | 56 | 8 | reserved, zero |

/// Size of every control message on the wire.
pub const CONTROL_LEN: usize = 64;
/// The only supported wire version.
pub const CONTROL_VERSION: u8 = 1;

const KIND_GRANT: u8 = 1;
const KIND_RELEASE: u8 = 2;
const KIND_CREDIT: u8 = 3;
const KIND_CLOSE: u8 = 4;

/// Message body by kind.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ControlKind {
    /// Receiver exposes `length` bytes at `addr` under `rkey` for `tag`.
    Grant {
        tag: u32,
        rkey: u32,
        addr: u64,
        length: u32,
        transfer_id: [u8; 16],
    },
    /// Sender declines to write for `tag`.
    Release { tag: u32, transfer_id: [u8; 16] },
    /// Standalone credit return.
    Credit,
    /// Orderly link close.
    Close,
}

/// A decoded control message.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ControlMessage {
    /// Receives the sender reposted since its last report.
    pub credits: u32,
    /// Link id agreed in the handshake.
    pub link_id: u64,
    pub kind: ControlKind,
}

/// Why a control message was rejected.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ControlError {
    Length(usize),
    Version(u8),
    Kind(u8),
    /// Byte offset of the first nonzero reserved byte.
    Reserved(usize),
    LinkId(u64),
    /// Zero tag on Grant or Release.
    ZeroTag,
    /// Zero grant length.
    ZeroLength,
    /// Grant length above the local buffer length.
    LengthTooLarge(u32),
    /// A field that must be zero for this kind is not (byte offset).
    FieldForKind(usize),
}

impl std::fmt::Display for ControlError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Length(n) => write!(f, "control message length {n}, want {CONTROL_LEN}"),
            Self::Version(v) => write!(f, "control message version {v} unsupported"),
            Self::Kind(k) => write!(f, "control message kind {k} unknown"),
            Self::Reserved(o) => write!(f, "control message reserved byte {o} nonzero"),
            Self::LinkId(id) => write!(f, "control message link id {id:#x} mismatch"),
            Self::ZeroTag => f.write_str("control message tag is zero"),
            Self::ZeroLength => f.write_str("grant length is zero"),
            Self::LengthTooLarge(n) => write!(f, "grant length {n} exceeds buffer"),
            Self::FieldForKind(o) => write!(f, "control message field at {o} invalid for kind"),
        }
    }
}

impl std::error::Error for ControlError {}

impl ControlMessage {
    /// Encode to the fixed wire layout. Does not validate; callers construct
    /// valid messages, and `decode` on the peer enforces the rules.
    pub fn encode(&self) -> [u8; CONTROL_LEN] {
        let mut b = [0u8; CONTROL_LEN];
        b[0] = CONTROL_VERSION;
        b[4..8].copy_from_slice(&self.credits.to_be_bytes());
        b[8..16].copy_from_slice(&self.link_id.to_be_bytes());

        match self.kind {
            ControlKind::Grant {
                tag,
                rkey,
                addr,
                length,
                transfer_id,
            } => {
                b[1] = KIND_GRANT;
                b[16..20].copy_from_slice(&tag.to_be_bytes());
                b[20..24].copy_from_slice(&rkey.to_be_bytes());
                b[24..32].copy_from_slice(&addr.to_be_bytes());
                b[32..36].copy_from_slice(&length.to_be_bytes());
                b[40..56].copy_from_slice(&transfer_id);
            }
            ControlKind::Release { tag, transfer_id } => {
                b[1] = KIND_RELEASE;
                b[16..20].copy_from_slice(&tag.to_be_bytes());
                b[40..56].copy_from_slice(&transfer_id);
            }
            ControlKind::Credit => b[1] = KIND_CREDIT,
            ControlKind::Close => b[1] = KIND_CLOSE,
        }

        b
    }

    /// Decode and validate. `bytes` is the received payload (its length is the
    /// completion's `byte_len`). `buffer_len` bounds a `Grant` length.
    pub fn decode(bytes: &[u8], link_id: u64, buffer_len: u32) -> Result<Self, ControlError> {
        let b: &[u8; CONTROL_LEN] = bytes
            .try_into()
            .map_err(|_| ControlError::Length(bytes.len()))?;

        if b[0] != CONTROL_VERSION {
            return Err(ControlError::Version(b[0]));
        }

        let kind = b[1];
        if !(KIND_GRANT..=KIND_CLOSE).contains(&kind) {
            return Err(ControlError::Kind(kind));
        }

        for range in [2..4, 36..40, 56..64] {
            if let Some(i) = range.clone().find(|&i| b[i] != 0) {
                return Err(ControlError::Reserved(i));
            }
        }

        let got_link = u64_at(b, 8);
        if got_link != link_id {
            return Err(ControlError::LinkId(got_link));
        }

        let credits = u32_at(b, 4);
        let tag = u32_at(b, 16);
        let mut transfer_id = [0u8; 16];
        transfer_id.copy_from_slice(&b[40..56]);

        let zero = |from: usize, to: usize| match (from..to).find(|&i| b[i] != 0) {
            Some(i) => Err(ControlError::FieldForKind(i)),
            None => Ok(()),
        };

        let kind = match kind {
            KIND_GRANT => {
                if tag == 0 {
                    return Err(ControlError::ZeroTag);
                }

                let length = u32_at(b, 32);
                if length == 0 {
                    return Err(ControlError::ZeroLength);
                }

                if length > buffer_len {
                    return Err(ControlError::LengthTooLarge(length));
                }

                ControlKind::Grant {
                    tag,
                    rkey: u32_at(b, 20),
                    addr: u64_at(b, 24),
                    length,
                    transfer_id,
                }
            }
            KIND_RELEASE => {
                if tag == 0 {
                    return Err(ControlError::ZeroTag);
                }

                zero(20, 36)?;

                ControlKind::Release { tag, transfer_id }
            }
            _ => {
                zero(16, 36)?;
                zero(40, 56)?;

                if kind == KIND_CREDIT {
                    ControlKind::Credit
                } else {
                    ControlKind::Close
                }
            }
        };

        Ok(Self {
            credits,
            link_id,
            kind,
        })
    }
}

fn u32_at(b: &[u8; CONTROL_LEN], o: usize) -> u32 {
    u32::from_be_bytes([b[o], b[o + 1], b[o + 2], b[o + 3]])
}

fn u64_at(b: &[u8; CONTROL_LEN], o: usize) -> u64 {
    let mut a = [0u8; 8];
    a.copy_from_slice(&b[o..o + 8]);
    u64::from_be_bytes(a)
}

#[cfg(test)]
mod tests {
    use super::*;

    const LINK: u64 = 0x0102_0304_0506_0708;
    const BUF: u32 = 1 << 20;
    const TID: [u8; 16] = [
        0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae,
        0xaf,
    ];

    fn grant() -> ControlMessage {
        ControlMessage {
            credits: 0x1122_3344,
            link_id: LINK,
            kind: ControlKind::Grant {
                tag: 0x5566_7788,
                rkey: 0x99aa_bbcc,
                addr: 0xdead_beef_cafe_f00d,
                length: BUF,
                transfer_id: TID,
            },
        }
    }

    fn all() -> Vec<ControlMessage> {
        let mk = |kind| ControlMessage {
            credits: 7,
            link_id: LINK,
            kind,
        };

        vec![
            grant(),
            mk(ControlKind::Release {
                tag: 1,
                transfer_id: TID,
            }),
            mk(ControlKind::Credit),
            mk(ControlKind::Close),
        ]
    }

    #[test]
    fn round_trip_every_kind() {
        for m in all() {
            assert_eq!(ControlMessage::decode(&m.encode(), LINK, BUF), Ok(m));
        }
    }

    #[test]
    fn exact_grant_layout() {
        let b = grant().encode();
        let mut want = [0u8; 64];
        want[0] = 1;
        want[1] = 1;
        want[4..8].copy_from_slice(&[0x11, 0x22, 0x33, 0x44]);
        want[8..16].copy_from_slice(&[1, 2, 3, 4, 5, 6, 7, 8]);
        want[16..20].copy_from_slice(&[0x55, 0x66, 0x77, 0x88]);
        want[20..24].copy_from_slice(&[0x99, 0xaa, 0xbb, 0xcc]);
        want[24..32].copy_from_slice(&[0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0xf0, 0x0d]);
        want[32..36].copy_from_slice(&[0, 0x10, 0, 0]);
        want[40..56].copy_from_slice(&TID);
        assert_eq!(b, want);
    }

    #[test]
    fn exact_kind_bytes() {
        let kinds: Vec<u8> = all().iter().map(|m| m.encode()[1]).collect();
        assert_eq!(kinds, [1, 2, 3, 4]);

        let close = all()[3].encode();
        assert!(close[16..].iter().all(|&x| x == 0));
        assert_eq!(&close[4..8], &[0, 0, 0, 7]);
    }

    #[test]
    fn rejects_wrong_length() {
        let b = grant().encode();
        for n in [0, 1, 63, 65, 128] {
            let mut v = b.to_vec();
            v.resize(n, 0);
            assert_eq!(
                ControlMessage::decode(&v, LINK, BUF),
                Err(ControlError::Length(n))
            );
        }
    }

    #[test]
    fn rejects_version_and_kind() {
        for v in [0u8, 2, 255] {
            let mut b = grant().encode();
            b[0] = v;
            assert_eq!(
                ControlMessage::decode(&b, LINK, BUF),
                Err(ControlError::Version(v))
            );
        }

        for k in [0u8, 5, 255] {
            let mut b = grant().encode();
            b[1] = k;
            assert_eq!(
                ControlMessage::decode(&b, LINK, BUF),
                Err(ControlError::Kind(k))
            );
        }
    }

    #[test]
    fn rejects_every_reserved_byte() {
        for m in all() {
            for i in (2..4).chain(36..40).chain(56..64) {
                let mut b = m.encode();
                b[i] = 0x80;
                assert_eq!(
                    ControlMessage::decode(&b, LINK, BUF),
                    Err(ControlError::Reserved(i))
                );
            }
        }
    }

    #[test]
    fn rejects_link_id() {
        for m in all() {
            assert_eq!(
                ControlMessage::decode(&m.encode(), LINK ^ 1, BUF),
                Err(ControlError::LinkId(LINK))
            );
        }
    }

    #[test]
    fn grant_bounds() {
        let with_len = |length| {
            let mut m = grant();
            if let ControlKind::Grant { length: l, .. } = &mut m.kind {
                *l = length;
            }
            m
        };

        assert_eq!(
            ControlMessage::decode(&with_len(0).encode(), LINK, BUF),
            Err(ControlError::ZeroLength)
        );
        assert!(ControlMessage::decode(&with_len(1).encode(), LINK, BUF).is_ok());
        assert!(ControlMessage::decode(&with_len(BUF).encode(), LINK, BUF).is_ok());
        assert_eq!(
            ControlMessage::decode(&with_len(BUF + 1).encode(), LINK, BUF),
            Err(ControlError::LengthTooLarge(BUF + 1))
        );
        assert!(ControlMessage::decode(&with_len(u32::MAX).encode(), LINK, u32::MAX).is_ok());
        assert_eq!(
            ControlMessage::decode(&with_len(1).encode(), LINK, 0),
            Err(ControlError::LengthTooLarge(1))
        );
    }

    #[test]
    fn rejects_zero_tag() {
        for mut m in all().into_iter().take(2) {
            match &mut m.kind {
                ControlKind::Grant { tag, .. } | ControlKind::Release { tag, .. } => *tag = 0,
                _ => unreachable!(),
            }
            assert_eq!(
                ControlMessage::decode(&m.encode(), LINK, BUF),
                Err(ControlError::ZeroTag)
            );
        }
    }

    #[test]
    fn boundary_values_round_trip() {
        let m = ControlMessage {
            credits: u32::MAX,
            link_id: u64::MAX,
            kind: ControlKind::Grant {
                tag: u32::MAX,
                rkey: 0,
                addr: u64::MAX,
                length: u32::MAX,
                transfer_id: [0xff; 16],
            },
        };
        assert_eq!(
            ControlMessage::decode(&m.encode(), u64::MAX, u32::MAX),
            Ok(m)
        );

        let m = ControlMessage {
            credits: 0,
            link_id: 0,
            kind: ControlKind::Release {
                tag: 1,
                transfer_id: [0; 16],
            },
        };
        assert_eq!(ControlMessage::decode(&m.encode(), 0, 0), Ok(m));
    }

    #[test]
    fn rejects_fields_invalid_for_kind() {
        let release = all()[1];
        for i in 20..36 {
            let mut b = release.encode();
            b[i] = 1;
            assert_eq!(
                ControlMessage::decode(&b, LINK, BUF),
                Err(ControlError::FieldForKind(i))
            );
        }

        for m in &all()[2..] {
            for i in (16..36).chain(40..56) {
                let mut b = m.encode();
                b[i] = 1;
                assert_eq!(
                    ControlMessage::decode(&b, LINK, BUF),
                    Err(ControlError::FieldForKind(i))
                );
            }
        }
    }

    /// Random inputs never yield an accepted Grant with bad fields.
    #[test]
    fn random_inputs_never_accept_bad_grant() {
        let mut s: u64 = 0x9e37_79b9_7f4a_7c15;
        let mut next = || {
            s ^= s << 13;
            s ^= s >> 7;
            s ^= s << 17;
            s
        };

        for i in 0..200_000 {
            let mut b = [0u8; 64];
            for chunk in b.chunks_mut(8) {
                chunk.copy_from_slice(&next().to_le_bytes());
            }

            // Bias half the inputs toward a valid header so field checks run.
            if i % 2 == 0 {
                b[0] = 1;
                b[1] = (next() % 5) as u8;
                b[2..4].fill(0);
                b[36..40].fill(0);
                b[56..64].fill(0);
                b[8..16].copy_from_slice(&LINK.to_be_bytes());
            }

            let buf = (next() as u32) >> (next() % 32);
            if let Ok(m) = ControlMessage::decode(&b, LINK, buf) {
                assert_eq!(m.link_id, LINK);
                assert_eq!(m.encode(), b);
                if let ControlKind::Grant { tag, length, .. } = m.kind {
                    assert_ne!(tag, 0);
                    assert!(length != 0 && length <= buf);
                }
            }
        }
    }
}
