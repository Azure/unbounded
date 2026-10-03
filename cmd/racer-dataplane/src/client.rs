//! Validate v2 subscriptions and HEAD metadata while preserving opaque fields.
//!
//! Wire values follow pkg/racersdk: canonical lowercase keys, quoted strong pins,
//! signed-63-bit decimal ranges, and byte-preserving opaque context.
pub mod listener;
pub mod response;

use crate::runtime::collections::HashSet;
use crate::{
    error::{Error, Result},
    http::{MessageHead, StartLine, is_token, trim_ows},
    model::{
        Authorization, ByteRange, CacheId, CacheKey, ObjectId, OpaqueMetadata, OriginContext,
        PAGE_BYTES, StrongEtag,
    },
};

pub use crate::{http::MAX_HEAD_BYTES, model::MAX_FIELD_BYTES};

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReadKind {
    Head,
    HeadPinned {
        etag: StrongEtag,
    },
    Subscription {
        pin: Option<StrongEtag>,
        range: Option<ByteRange>,
        page_credits: usize,
        byte_credits: u64,
        ordered: bool,
    },
}

impl ReadKind {
    pub fn is_head(&self) -> bool {
        matches!(self, Self::Head | Self::HeadPinned { .. })
    }

    pub fn pin(&self) -> Option<&StrongEtag> {
        match self {
            Self::HeadPinned { etag } => Some(etag),
            Self::Subscription { pin, .. } => pin.as_ref(),
            _ => None,
        }
    }
}

pub struct ClientRequest {
    pub kind: ReadKind,
    pub origin: OriginContext,
}

#[derive(Clone)]
pub struct RequestParser {
    header_limit: usize,
}
impl RequestParser {
    pub fn new(header_limit: usize) -> Self {
        Self {
            header_limit: header_limit.min(MAX_HEAD_BYTES),
        }
    }
    /// Apply this cap to raw HTTP framing before calling the semantic parser.
    pub(crate) fn header_limit(&self) -> usize {
        self.header_limit
    }
    /// Codec must validate the raw head and its byte limit before calling this.
    /// In particular, context fields must have exactly one separator space and
    /// must not have their value trimmed by the codec. Decoded fields cannot
    /// reconstruct wire length: unknown fields need not have a separator SP.
    pub fn parse(&self, cache: &CacheId, head: MessageHead) -> Result<ClientRequest> {
        let StartLine::Request { method, target } = head.start else {
            return Err(Error::InvalidRequest.into());
        };
        // Bound manually constructed input data as well, without pretending this
        // is a wire-length check. Framing owns start-line/colon/OWS/CRLF accounting.
        let mut decoded_bytes = method.len().saturating_add(target.len());
        let mut seen = HashSet::default();
        let mut host = None;
        let mut pin = None;
        let mut range = None;
        let mut metadata = None;
        let mut authorization = None;
        let mut content_length = false;
        let mut page_credits = 2;
        let mut byte_credits = 2 * PAGE_BYTES;
        let mut ordered = false;
        for header in head.headers {
            decoded_bytes = decoded_bytes
                .saturating_add(header.name.len())
                .saturating_add(header.value.len());
            if decoded_bytes > self.header_limit {
                return Err(Error::HeaderTooLarge);
            }
            if header.name.is_empty()
                || !header.name.bytes().all(is_token)
                || header
                    .value
                    .iter()
                    .any(|&b| b == 0x7f || (b < 0x20 && b != b'\t'))
            {
                return Err(Error::InvalidRequest.into());
            }
            let name = header.name.to_ascii_lowercase();
            let value = header.value.as_slice();
            if matches!(
                name.as_str(),
                "host"
                    | "content-length"
                    | "content-type"
                    | "content-range"
                    | "etag"
                    | "if-match"
                    | "range"
                    | "racer-expires-at"
                    | "racer-content-type"
                    | "racer-metadata"
                    | "authorization"
                    | "racer-page-credits"
                    | "racer-byte-credits"
                    | "racer-ordered"
            ) && !seen.insert(name.clone())
            {
                return Err(Error::InvalidRequest.into());
            }
            match name.as_str() {
                "host" => host = Some(value == b"racer"),
                "content-length" if value == b"0" => content_length = true,
                "content-length"
                | "content-range"
                | "etag"
                | "racer-expires-at"
                | "racer-content-type"
                | "transfer-encoding"
                | "content-encoding"
                | "trailer"
                | "upgrade"
                | "expect"
                | "if-none-match"
                | "if-modified-since"
                | "if-unmodified-since"
                | "if-range" => return Err(Error::InvalidRequest.into()),
                "connection"
                    if value
                        .split(|&b| b == b',')
                        .any(|token| trim_ows(token).eq_ignore_ascii_case(b"upgrade")) =>
                {
                    return Err(Error::InvalidRequest.into());
                }
                "if-match" => {
                    if value.len() > MAX_FIELD_BYTES {
                        return Err(Error::HeaderTooLarge);
                    }
                    pin = Some(StrongEtag::parse(value)?);
                }
                "range" => range = Some(ByteRange::parse(value)?),
                "racer-page-credits" => page_credits = decimal(value, 1, 64)? as usize,
                "racer-byte-credits" => {
                    byte_credits = decimal(value, PAGE_BYTES, 64 * PAGE_BYTES)?;
                }
                "racer-ordered" => {
                    ordered = match value {
                        b"0" => false,
                        b"1" => true,
                        _ => return Err(Error::InvalidRequest.into()),
                    }
                }
                "racer-metadata" => {
                    validate_opaque(value)?;
                    metadata = Some(OpaqueMetadata::from_header(value)?);
                }
                "authorization" => {
                    validate_opaque(value)?;
                    authorization = Some(Authorization::from_header(value)?);
                }
                _ => {}
            }
        }
        if decoded_bytes > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        if host != Some(true) {
            return Err(Error::InvalidRequest.into());
        }
        let key = CacheKey::parse_hex(
            target
                .strip_prefix("/v2/objects/")
                .ok_or(Error::InvalidRequest)?
                .as_bytes(),
        )?;
        let kind = match method.as_str() {
            "HEAD" if range.is_none() => match pin {
                Some(etag) => ReadKind::HeadPinned { etag },
                None => ReadKind::Head,
            },
            "HEAD" => return Err(Error::InvalidRequest.into()),
            "POST" if content_length => ReadKind::Subscription {
                pin,
                range,
                page_credits,
                byte_credits,
                ordered,
            },
            "POST" => return Err(Error::InvalidRequest.into()),
            _ => return Err(Error::MethodNotAllowed),
        };
        Ok(ClientRequest {
            kind,
            origin: OriginContext {
                object: ObjectId {
                    cache: cache.clone(),
                    key,
                },
                metadata,
                authorization,
            },
        })
    }
}

fn decimal(value: &[u8], minimum: u64, maximum: u64) -> Result<u64> {
    if value.is_empty() || value[0] == b'0' || !value.iter().all(u8::is_ascii_digit) {
        return Err(Error::InvalidRequest);
    }
    let number = value
        .iter()
        .try_fold(0u64, |n, digit| {
            n.checked_mul(10)?.checked_add(u64::from(digit - b'0'))
        })
        .ok_or(Error::InvalidRequest)?;
    if !(minimum..=maximum).contains(&number) {
        return Err(Error::InvalidRequest);
    }
    Ok(number)
}

fn validate_opaque(value: &[u8]) -> Result<()> {
    if value.len() > MAX_FIELD_BYTES {
        return Err(Error::HeaderTooLarge);
    }
    if value.is_empty()
        || value.first() == Some(&b' ')
        || value.last() == Some(&b' ')
        || value.iter().any(|&b| b < 0x20 || b == 0x7f)
    {
        return Err(Error::InvalidRequest.into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::{Codec, Header};

    fn head(method: &str, fields: &[(&str, &[u8])]) -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: format!("/v2/objects/{}", "01".repeat(32)),
            },
            headers: std::iter::once(Header {
                name: "Host".into(),
                value: b"racer".to_vec(),
            })
            .chain(fields.iter().map(|(name, value)| Header {
                name: (*name).into(),
                value: value.to_vec(),
            }))
            .chain((method == "POST").then(|| Header {
                name: "Content-Length".into(),
                value: b"0".to_vec(),
            }))
            .collect(),
        }
    }

    fn parse(head: MessageHead) -> Result<ClientRequest> {
        RequestParser::new(MAX_HEAD_BYTES).parse(&CacheId("uid".into()), head)
    }

    #[test]
    fn sdk_methods_ranges_pins_and_opaque_bytes() {
        for (method, fields, expected) in [
            ("HEAD", vec![], ReadKind::Head),
            (
                "HEAD",
                vec![("If-Match", b"\"\"".as_slice())],
                ReadKind::HeadPinned {
                    etag: StrongEtag::parse(b"\"\"").unwrap(),
                },
            ),
            (
                "POST",
                vec![("Range", b"bytes=0-16777215".as_slice())],
                ReadKind::Subscription {
                    pin: None,
                    range: Some(ByteRange::Closed {
                        first: 0,
                        last: PAGE_BYTES - 1,
                    }),
                    page_credits: 2,
                    byte_credits: 2 * PAGE_BYTES,
                    ordered: false,
                },
            ),
            (
                "POST",
                vec![
                    ("Range", b"bytes=-0".as_slice()),
                    ("If-Match", b"\"a,b\\c\"".as_slice()),
                ],
                ReadKind::Subscription {
                    pin: Some(StrongEtag::parse(b"\"a,b\\c\"").unwrap()),
                    range: Some(ByteRange::Suffix(0)),
                    page_credits: 2,
                    byte_credits: 2 * PAGE_BYTES,
                    ordered: false,
                },
            ),
            (
                "POST",
                vec![
                    ("Range", b"bytes=16777216-".as_slice()),
                    ("If-Match", b"\"v\"".as_slice()),
                ],
                ReadKind::Subscription {
                    pin: Some(StrongEtag::parse(b"\"v\"").unwrap()),
                    range: Some(ByteRange::From(PAGE_BYTES)),
                    page_credits: 2,
                    byte_credits: 2 * PAGE_BYTES,
                    ordered: false,
                },
            ),
        ] {
            let mut request = head(method, &fields);
            request.headers.push(Header {
                name: "Racer-Metadata".into(),
                value: b"opaque,\xff value".to_vec(),
            });
            request.headers.push(Header {
                name: "Authorization".into(),
                value: b"Scheme opaque \xfe".to_vec(),
            });
            let request = parse(request).unwrap();
            assert_eq!(request.kind, expected);
            assert_eq!(request.origin.object.key, CacheKey([1; 32]));
            assert_eq!(
                request.origin.metadata.unwrap().as_header(),
                b"opaque,\xff value"
            );
            assert_eq!(
                request.origin.authorization.unwrap().expose_for_origin(),
                b"Scheme opaque \xfe"
            );
        }
    }

    #[test]
    fn rejects_noncanonical_targets_and_envelopes() {
        for target in [
            format!("/v1/objects/{}?", "0".repeat(64)),
            format!("/v1/objects/{}", "A".repeat(64)),
            format!("http://racer/v1/objects/{}", "0".repeat(64)),
            "/v1/objects/%30".into(),
        ] {
            let mut request = head("HEAD", &[]);
            request.start = StartLine::Request {
                method: "HEAD".into(),
                target,
            };
            assert!(parse(request).is_err());
        }
        for (name, value) in [
            ("Host", b"racer".as_slice()),
            ("Content-Length", b"00"),
            ("Expect", b"100-continue"),
            ("Transfer-Encoding", b"identity"),
            ("Content-Encoding", b"identity"),
            ("If-Range", b"\"v\""),
            ("Connection", b"keep-alive, Upgrade"),
            ("Range", b"bytes=0-0"),
        ] {
            assert!(parse(head("HEAD", &[(name, value)])).is_err());
        }
        assert!(matches!(
            parse(head("GET", &[])),
            Err(Error::MethodNotAllowed)
        ));
        let mut request = head("HEAD", &[]);
        request.headers.clear();
        assert!(parse(request).is_err());
    }

    #[test]
    fn rejects_ambiguous_pins_and_ranges() {
        for pin in [
            b"*".as_slice(),
            b"W/\"v\"",
            b"\"a\", \"b\"",
            b"",
            b"\"space value\"",
        ] {
            assert!(parse(head("HEAD", &[("If-Match", pin)])).is_err());
        }
        for range in [
            b"bytes=00-1".as_slice(),
            b"bytes=1-0",
            b"bytes=0-1,2-3",
            b"bytes=9223372036854775808-",
            b"bytes=-",
            b"bytes=+1-2",
            b"bytes=0--1",
        ] {
            assert!(parse(head("POST", &[("Range", range), ("If-Match", b"\"v\"")])).is_err());
        }
        for range in [b"bytes=0-1".as_slice(), b"bytes=0-", b"bytes=-1"] {
            assert!(parse(head("POST", &[("Range", range)])).is_ok());
        }
        assert!(parse(head("GET", &[])).is_err());
    }

    #[test]
    fn subscription_credit_limits_defaults_and_required_zero_body() {
        assert_eq!(
            parse(head("POST", &[])).unwrap().kind,
            ReadKind::Subscription {
                pin: None,
                range: None,
                page_credits: 2,
                byte_credits: 2 * PAGE_BYTES,
                ordered: false,
            }
        );
        for name in ["Racer-Page-Credits", "Racer-Byte-Credits"] {
            for value in [
                b"".as_slice(),
                b"0",
                b"00",
                b"01",
                b"+1",
                b"-1",
                b" 1",
                b"1 ",
                b"1.0",
                b"18446744073709551616",
            ] {
                assert!(
                    parse(head("POST", &[(name, value)])).is_err(),
                    "{name} {value:?}"
                );
            }
            assert!(parse(head("POST", &[(name, b"2"), (name, b"2")])).is_err());
        }
        for pages in [1, 64] {
            for bytes in [PAGE_BYTES, 64 * PAGE_BYTES] {
                for ordered in ["0", "1"] {
                    let parsed = parse(head(
                        "POST",
                        &[
                            ("Racer-Page-Credits", pages.to_string().as_bytes()),
                            ("Racer-Byte-Credits", bytes.to_string().as_bytes()),
                            ("Racer-Ordered", ordered.as_bytes()),
                        ],
                    ))
                    .unwrap();
                    assert!(
                        matches!(parsed.kind, ReadKind::Subscription { page_credits, byte_credits, ordered: actual, .. }
                        if page_credits == pages && byte_credits == bytes && actual == (ordered == "1"))
                    );
                }
            }
        }
        for (name, value) in [
            ("Racer-Page-Credits", "65".into()),
            ("Racer-Byte-Credits", (PAGE_BYTES - 1).to_string()),
            ("Racer-Byte-Credits", (64 * PAGE_BYTES + 1).to_string()),
            ("Racer-Ordered", "2".into()),
            ("Racer-Ordered", "01".into()),
        ] {
            assert!(parse(head("POST", &[(name, value.as_bytes())])).is_err());
        }
        assert!(
            parse(head(
                "POST",
                &[("Racer-Ordered", b"0"), ("racer-ordered", b"1")]
            ))
            .is_err()
        );
        let mut missing = head("POST", &[]);
        missing.headers.retain(|h| h.name != "Content-Length");
        assert!(parse(missing).is_err());
        for value in [b"0".as_slice(), b"00", b"1"] {
            assert!(parse(head("POST", &[("Content-Length", value)])).is_err());
        }
        let mut legacy = head("POST", &[]);
        if let StartLine::Request { target, .. } = &mut legacy.start {
            *target = target.replace("/v2/", "/v1/");
        }
        assert!(parse(legacy).is_err());
    }

    #[test]
    fn context_duplicates_empty_and_limits() {
        for name in ["Authorization", "Racer-Metadata"] {
            for value in [
                b"".as_slice(),
                b" leading",
                b"trailing ",
                b"a\tb",
                b"a\x7fb",
                b"a\rb",
            ] {
                assert!(parse(head("HEAD", &[(name, value)])).is_err());
            }
            assert!(
                parse(head(
                    "HEAD",
                    &[(name, b"a"), (&name.to_ascii_lowercase(), b"a")]
                ))
                .is_err()
            );
            assert!(parse(head("HEAD", &[(name, &vec![b'a'; MAX_FIELD_BYTES])])).is_ok());
            assert!(matches!(
                parse(head("HEAD", &[(name, &vec![b'a'; MAX_FIELD_BYTES + 1])])),
                Err(Error::HeaderTooLarge)
            ));
        }
        let request = head("HEAD", &[("X", &vec![b'x'; MAX_HEAD_BYTES])]);
        assert!(matches!(parse(request), Err(Error::HeaderTooLarge)));
    }

    #[test]
    fn sdk_total_head_limit_includes_unknown_fields() {
        check_raw_head_limits(&[MAX_HEAD_BYTES], &[" "], &[""]);
    }

    #[test]
    fn head_rejects_legacy_endpoint() {
        let codec = Codec::new(MAX_HEAD_BYTES);
        for endpoint in ["v1", "v2"] {
            let raw = format!(
                "HEAD /{endpoint}/objects/{} HTTP/1.1\r\nHost: racer\r\n\r\n",
                "0".repeat(64)
            );
            let (head, _) = codec.decode_head(raw.as_bytes()).unwrap().unwrap();
            assert_eq!(parse(head).is_ok(), endpoint == "v2");
        }
    }

    #[test]
    fn raw_head_limit_is_independent_of_unknown_field_whitespace() {
        check_raw_head_limits(
            &[MAX_HEAD_BYTES],
            &["", " ", "\t", "  ", " \t"],
            &["", " ", "\t"],
        );
    }

    #[test]
    fn sdk_raw_head_preserves_non_utf8_and_rejects_normalization() {
        let codec = Codec::new(MAX_HEAD_BYTES);
        let mut raw = format!(
            "HEAD /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nRacer-Metadata: opaque,",
            "0".repeat(64)
        )
        .into_bytes();
        raw.extend_from_slice(b"\xff value\r\nAuthorization: Bearer secret\r\n\r\n");
        let (head, used) = codec.decode_head(&raw).unwrap().unwrap();
        assert_eq!(used, raw.len());
        let parsed = parse(head).unwrap();
        assert_eq!(
            parsed.origin.metadata.unwrap().as_header(),
            b"opaque,\xff value"
        );
        for value in [
            "",
            "secret",
            "  secret",
            " secret ",
            "\tsecret",
            " secret\t",
        ] {
            let raw = format!(
                "HEAD /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nAuthorization:{value}\r\n\r\n",
                "0".repeat(64)
            );
            let rejected = match codec.decode_head(raw.as_bytes()) {
                Err(_) => true,
                Ok(Some((head, _))) => parse(head).is_err(),
                Ok(None) => false,
            };
            assert!(rejected, "opaque separator/OWS was normalized");
        }
    }

    #[test]
    fn raw_head_limit_does_not_assume_unknown_header_whitespace() {
        check_raw_head_limits(&[512], &["", " ", "\t", "  ", " \t"], &["", " ", "\t"]);
    }

    fn check_raw_head_limits(limits: &[usize], separators: &[&str], trailing_values: &[&str]) {
        for separator in separators {
            for trailing in trailing_values {
                for &limit in limits {
                    let parser = RequestParser::new(limit);
                    let codec = Codec::new(parser.header_limit());
                    let prefix = format!(
                        "HEAD /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nX:{separator}",
                        "0".repeat(64)
                    );
                    let suffix = format!("{trailing}\r\nY:\r\n\r\n");
                    for length in [limit - 1, limit, limit + 1] {
                        let raw = format!(
                            "{prefix}{}{suffix}",
                            "x".repeat(length - prefix.len() - suffix.len())
                        );
                        assert_eq!(raw.len(), length);
                        let decoded = codec.decode_head(raw.as_bytes());
                        if length > limit {
                            assert!(matches!(decoded, Err(http1::Error::HeadTooLarge)));
                        } else {
                            let (head, consumed) = decoded.unwrap().unwrap();
                            assert_eq!(consumed, length);
                            assert!(parser.parse(&CacheId("uid".into()), head).is_ok());
                        }
                    }
                }
            }
        }
    }
}
#[cfg(test)]
pub(crate) mod test_support {
    //! Real read ownership behind client wire scenarios. Only the UDS adapter is scripted.
    use crate::{
        control::{
            state::Publication,
            state::{CacheDefinition, PublishedState, SnapshotStore},
        },
        memory::{BufferPool, cache::MemoryCache, delivery::Delivery},
        model::{MembershipVersion, ObjectMetadata, WorkerId},
        read::{
            Coordinator,
            candidates::CandidatePolicy,
            dispatch::{WorkerDirectory, WorkerEndpoint},
            drivers::DriverQueue,
            fill::{Fill, FillDependencies},
            flight::Flights,
            metadata::{MetadataDependencies, MetadataService},
            range_stream::RangeStreams,
        },
        runtime::{
            admission::AdmissionPolicy,
            crypto::{self, CryptoClient},
            reactor::Reactor,
            worker::{CryptoRuntime, CryptoService, WorkerMap},
        },
        security::{
            aead::{PageCrypto, PageCryptoEngine},
            credentials::CredentialCrypto,
        },
        store::{
            StoreReader, StoreWriter,
            catalog::{Index, SegmentClock},
        },
        test_support::origin::AdapterOrigin,
        topology::{membership::Member, routing::Placement},
    };
    use racer_control_wire::PublicationSequence;
    use std::{cell::RefCell, num::NonZeroU32, rc::Rc, sync::Arc, task::Context};

    pub(crate) struct ReadWorker {
        pub coordinator: Rc<Coordinator>,
        pub streams: Rc<RangeStreams>,
        pub membership: crate::topology::membership::MembershipLease,
        pub origin: AdapterOrigin,
        pub drivers: Rc<DriverQueue>,
        endpoint: RefCell<WorkerEndpoint>,
        engine: RefCell<PageCryptoEngine>,
        crypto: Rc<CryptoClient>,
        writer: Rc<StoreWriter>,
        memory: Rc<MemoryCache>,
        cache: crate::model::CacheId,
    }

    impl ReadWorker {
        pub fn new(
            cache: CacheDefinition,
            metadata: ObjectMetadata,
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            reactor: Rc<Reactor>,
            delivery: Rc<Delivery>,
            window: usize,
        ) -> Self {
            let origin = AdapterOrigin::new(&cache.name, metadata);
            let keys = Rc::new(crate::security::test_support::keys());
            let publications = Arc::new(PublishedState::default());
            let availability = Rc::new(crate::control::state::Availability::new(
                publications.clone(),
                keys.clone(),
            ));
            let snapshots = Rc::new(SnapshotStore::new(keys.cluster().clone(), publications, 2));
            snapshots
                .publish(Publication {
                    schema_version: 1,
                    cluster: keys.cluster().clone(),
                    sequence: PublicationSequence(1),
                    membership_version: MembershipVersion(1),
                    members: vec![Member {
                        node: keys.node().clone(),
                        shares: NonZeroU32::new(1).unwrap(),
                        peer_endpoint: "127.0.0.1:1".into(),
                        rails: vec![],
                        site: String::new(),
                    }],
                    caches: vec![cache.clone()],
                })
                .unwrap();
            let directory = Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    16,
                )
                .unwrap(),
            );
            let buffers = BufferPool::new(admission.clone());
            let index = Rc::new(Index::new(WorkerId(0), 16, availability.clone()));
            let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
            // Reads use an empty disk index. No slabs need to be opened or written.
            let slabs = Rc::new(page_alloc::Slab::new(
                origin.root.join("slabs/worker-0-slab-0.dat"),
                256 * 1024 * 1024,
                64 * 1024 * 1024,
                crate::model::PAGE_BYTES as usize + crate::store::format::MAX_HEADER_BYTES + 16,
            ));
            let writer = Rc::new(StoreWriter::new(
                index.clone(),
                segments.clone(),
                slabs.clone(),
                admission.clone(),
                reactor.clone(),
                availability.clone(),
            ));
            let disk = Rc::new(StoreReader::new(
                Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1)),
                index.clone(),
                segments,
                slabs,
                admission.clone(),
                reactor.clone(),
                buffers.clone(),
            ));
            let (port, engine) =
                crypto::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(16).unwrap());
            let crypto = Rc::new(CryptoClient::new(port));
            let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
            let candidates = Rc::new(CandidatePolicy::new(
                keys.node().clone(),
                Rc::new(Placement::new(16)),
                Rc::new(crate::test_support::NoPeers),
                credentials.clone(),
                Arc::new(Default::default()),
            ));
            let client = origin.client(
                snapshots.clone(),
                admission.clone(),
                reactor,
                buffers.clone(),
            );
            let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
            let fill = Rc::new(Fill::new(FillDependencies {
                memory: memory.clone(),
                buffers,
                disk,
                writer: writer.clone(),
                origin: client.clone(),
                candidates: candidates.clone(),
                flights: Rc::new(Flights::new(admission.clone(), availability.clone())),
                crypto: Rc::new(PageCrypto::new(keys, crypto.clone())),
                credentials: credentials.clone(),
                admission,
                metadata_owner: directory.clone(),
            }));
            let metadata = Rc::new(MetadataService::new(
                candidates,
                client,
                credentials.clone(),
                16,
                MetadataDependencies {
                    index,
                    owners: directory.clone(),
                    fill: fill.clone(),
                },
            ));
            let streams = Rc::new(RangeStreams::new(directory.clone(), delivery, window));
            let membership = snapshots.current().unwrap().membership.clone();
            let coordinator = Rc::new(Coordinator::new(
                snapshots,
                metadata,
                fill,
                streams.clone(),
                credentials,
                availability,
            ));
            let endpoint = directory.install(WorkerId(0), coordinator.clone()).unwrap();
            Self {
                coordinator,
                streams,
                membership,
                origin,
                drivers: Rc::new(DriverQueue::default()),
                endpoint: RefCell::new(endpoint),
                engine: RefCell::new(PageCryptoEngine::new(CryptoRuntime { port: engine })),
                crypto,
                writer,
                memory,
                cache: cache.id,
            }
        }

        pub fn poll(&self, cx: &mut Context<'_>) {
            let _queue = self.drivers.enter();
            self.endpoint.borrow_mut().poll_budgeted(64).unwrap();
            self.drivers.poll(cx, 64);
            self.engine.borrow_mut().poll_budgeted(64).unwrap();
            self.crypto.poll_budgeted(64).unwrap();
            // Exercise acquisition/authentication, not persistence or cache retention.
            // Delivered leases remain charged even after the cache drops its copy.
            self.writer.discard_unsubmitted();
            self.memory.remove_cache(&self.cache).unwrap();
        }
    }
}
