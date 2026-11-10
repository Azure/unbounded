//! Codec, signature, and response-binding tests for the write peer operations.

use super::*;
use crate::peer::protocol::SecurityCodec;
use crate::peer::protocol::SessionId;
use crate::peer::protocol::WriteAction;
use crate::peer::protocol::WriteReply;
use crate::peer::protocol::WriteStore;
use crate::peer::protocol::decode_envelope;
use crate::peer::protocol::encode_envelope;
use crate::peer::protocol::request_head;
use crate::peer::protocol::write;
use racer_core::write::Durability;
use racer_core::write::Holders;
use racer_core::write::Manifest;
use racer_core::write::UploadMode;
use racer_core::write::UploadState;
use racer_core::write::WriteSpec;

const ETAG: &[u8] = b"\"version\"";
const SESSION: SessionId = SessionId([9; 16]);

fn etag() -> StrongEtag {
    StrongEtag::parse(ETAG).unwrap()
}

fn spec() -> WriteSpec {
    WriteSpec {
        length: 3 * PAGE_BYTES + 1,
        page_size: PAGE_BYTES,
        content_type: Some("application/x-test".into()),
        replicas: 2,
        durability: Durability::Replicas(2),
        upload: UploadMode::Auto,
        hold_until_millis: 1_900_000_000_000,
    }
}

fn holders() -> Holders {
    let id = |n: usize| node(n).0.into_bytes();
    Holders(vec![
        vec![id(1), id(2)],
        vec![id(2), id(3)],
        vec![id(1), id(3)],
        vec![id(1), id(2)],
    ])
}

fn manifest() -> Manifest {
    Manifest {
        session: SESSION,
        etag: "\"version\"".into(),
        spec: spec(),
        holders: holders(),
        state: UploadState::Pending,
        blob_etag: None,
    }
}

fn store(pages: Vec<u64>) -> WriteStore {
    WriteStore {
        session: SESSION,
        pages,
        length: 3 * PAGE_BYTES + 1,
        page_size: crate::model::PageSize::DEFAULT,
        content_type: Some(crate::model::ContentType::parse(b"application/x-test").unwrap()),
        source: node(0),
        hold_until_millis: 1_900_000_000_000,
    }
}

/// Every write action, with and without the optional version.
fn actions() -> Vec<(WriteAction, bool)> {
    vec![
        (
            WriteAction::Open {
                session: SESSION,
                spec: spec(),
            },
            true,
        ),
        (
            WriteAction::Open {
                session: SESSION,
                spec: WriteSpec {
                    content_type: None,
                    durability: Durability::Origin,
                    replicas: 1,
                    ..spec()
                },
            },
            true,
        ),
        (WriteAction::Renew { session: SESSION }, true),
        (
            WriteAction::Commit {
                session: SESSION,
                holders: holders(),
            },
            true,
        ),
        (WriteAction::Abort { session: SESSION }, true),
        (WriteAction::Status { wait_millis: 0 }, false),
        (
            WriteAction::Status {
                wait_millis: 30_000,
            },
            true,
        ),
        (WriteAction::Delete, false),
        (WriteAction::Delete, true),
        (WriteAction::Persist, false),
        (
            WriteAction::Replicate {
                manifest: manifest(),
            },
            true,
        ),
    ]
}

/// A request for `operation` with the fixed write mode's origin context.
fn write_request(env: &racer_io::Env, id: u8, operation: Operation) -> PeerRequest {
    let mut request = request(env, id);
    let acquire = matches!(
        operation,
        Operation::WriteSession { .. } | Operation::WriteUpload { .. }
    );
    if acquire {
        request.route.remaining_attempts = 4;
    } else {
        request.origin.metadata = None;
        request.origin.authorization = None;
    }
    request.route.destination = node(1);
    request.operation = operation;
    request
}

fn version(request: &PeerRequest) -> ObjectVersion {
    ObjectVersion {
        object: request.origin.object.clone(),
        etag: etag(),
    }
}

/// Every write operation, built against `env`'s fixture object.
fn operations(env: &racer_io::Env) -> Vec<Operation> {
    let object = request(env, 1).origin.object;
    let version = ObjectVersion {
        object: object.clone(),
        etag: etag(),
    };
    let mut out: Vec<_> = actions()
        .into_iter()
        .map(|(action, versioned)| Operation::WriteSession {
            object: object.clone(),
            etag: versioned.then(etag),
            action: Box::new(action),
        })
        .collect();
    out.push(Operation::WriteStore {
        version: version.clone(),
        store: Box::new(store(vec![0, 2, 3])),
    });
    out.push(Operation::WriteStore {
        version: version.clone(),
        store: Box::new(WriteStore {
            content_type: None,
            ..store(vec![1])
        }),
    });
    out.push(Operation::WriteRelease {
        version: version.clone(),
        session: SESSION,
        pages: vec![0, 1, 2, 3],
    });
    out.push(Operation::WriteUpload {
        version,
        session: SESSION,
        pages: vec![2],
    });
    out
}

fn same(a: &Operation, b: &Operation) -> bool {
    match (a, b) {
        (
            Operation::WriteSession {
                object: o1,
                etag: e1,
                action: a1,
            },
            Operation::WriteSession {
                object: o2,
                etag: e2,
                action: a2,
            },
        ) => o1 == o2 && e1 == e2 && a1 == a2,
        (
            Operation::WriteStore {
                version: v1,
                store: s1,
            },
            Operation::WriteStore {
                version: v2,
                store: s2,
            },
        ) => v1 == v2 && s1 == s2,
        (
            Operation::WriteRelease {
                version: v1,
                session: s1,
                pages: p1,
            },
            Operation::WriteRelease {
                version: v2,
                session: s2,
                pages: p2,
            },
        )
        | (
            Operation::WriteUpload {
                version: v1,
                session: s1,
                pages: p1,
            },
            Operation::WriteUpload {
                version: v2,
                session: s2,
                pages: p2,
            },
        ) => v1 == v2 && s1 == s2 && p1 == p2,
        _ => false,
    }
}

fn clone_operation(operation: &Operation) -> Operation {
    match operation {
        Operation::WriteSession {
            object,
            etag,
            action,
        } => Operation::WriteSession {
            object: object.clone(),
            etag: etag.clone(),
            action: action.clone(),
        },
        Operation::WriteStore { version, store } => Operation::WriteStore {
            version: version.clone(),
            store: store.clone(),
        },
        Operation::WriteRelease {
            version,
            session,
            pages,
        } => Operation::WriteRelease {
            version: version.clone(),
            session: *session,
            pages: pages.clone(),
        },
        Operation::WriteUpload {
            version,
            session,
            pages,
        } => Operation::WriteUpload {
            version: version.clone(),
            session: *session,
            pages: pages.clone(),
        },
        _ => panic!("write operation expected"),
    }
}

fn codec() -> SecurityCodec {
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    SecurityCodec::new(admission.clone(), crate::memory::BufferPool::new(admission))
}

/// Decode `signed` through the wire envelope, as a receiver does.
fn decode(
    codec: &SecurityCodec,
    signed: &crate::peer::protocol::SignedRequest,
) -> Result<crate::peer::protocol::SignedRequest> {
    let scope = signed.request.origin.scope().clone();
    let (auth, _) = decode_envelope(
        encode_envelope(&signed.authentication, false, 0)?,
        false,
        &peer_wire::test_util::AnyChain,
    )?;
    codec.request(auth, &scope)
}

#[test]
fn write_operations_round_trip_signed_without_page_bytes() {
    let env = racer_io::Env::host();
    let signatures = network(&env, 2);
    let sender = Forwarding::new(signatures[0].clone());
    let receiver = Forwarding::new(signatures[1].clone());
    let codec = codec();
    for (i, operation) in operations(&env).into_iter().enumerate() {
        let expected = clone_operation(&operation);
        let request = write_request(&env, 40 + i as u8, operation);
        let head = request_head(&env.ctx.clock, &request).unwrap();
        assert_eq!(
            head.unique("content-length").unwrap(),
            Some(b"0".as_slice())
        );
        let mode = head.unique("racer-mode").unwrap().unwrap().to_vec();
        let acquire = matches!(
            expected,
            Operation::WriteSession { .. } | Operation::WriteUpload { .. }
        );
        assert_eq!(
            mode,
            if acquire {
                b"acquire".to_vec()
            } else {
                b"copy".to_vec()
            }
        );
        let (signed, _) = sender.sign_request_to(request, &node(1)).unwrap();
        let decoded = decode(&codec, &signed).unwrap();
        assert!(same(&decoded.request.operation, &expected), "operation {i}");
        assert!(decoded.request.operation.is_write());
        assert_eq!(
            decoded.request.operation.write_object(),
            Some(&decoded.request.origin.object)
        );
        // Sealed origin context survives only on acquire operations.
        assert_eq!(decoded.request.origin.authorization.is_some(), acquire);
        receiver.verify_request(signed).unwrap();
    }
}

#[test]
fn write_replies_round_trip_and_bind_to_their_request() {
    let env = racer_io::Env::host();
    let signatures = network(&env, 2);
    let sender = Forwarding::new(signatures[0].clone());
    let receiver = Forwarding::new(signatures[1].clone());
    let codec = codec();
    let base = request(&env, 1);
    let version = version(&base);
    let store_op = || Operation::WriteStore {
        version: version.clone(),
        store: Box::new(store(vec![0, 2, 3])),
    };
    let upload_op = || Operation::WriteUpload {
        version: version.clone(),
        session: SESSION,
        pages: vec![1, 2],
    };
    let session_op = || Operation::WriteSession {
        object: version.object.clone(),
        etag: None,
        action: Box::new(WriteAction::Status { wait_millis: 5 }),
    };
    let cases: Vec<(&dyn Fn() -> Operation, WriteReply)> = vec![
        (
            &session_op,
            WriteReply::Opened {
                lease_until_millis: 1_900_000_060_000,
            },
        ),
        (&store_op, WriteReply::Stored { pages: vec![0, 3] }),
        (
            &session_op,
            WriteReply::Committed {
                state: UploadState::Uploading,
            },
        ),
        (&store_op, WriteReply::Released),
        (&upload_op, WriteReply::Uploaded { pages: vec![1, 2] }),
        (
            &session_op,
            WriteReply::Status {
                state: UploadState::Committed,
                etag: "\"racer-1\"".into(),
            },
        ),
        (&session_op, WriteReply::Exists),
        (&store_op, WriteReply::Full),
        (&session_op, WriteReply::LeaseExpired),
        (&upload_op, WriteReply::Unknown),
        (&upload_op, WriteReply::Failed),
    ];
    for (i, (operation, reply)) in cases.into_iter().enumerate() {
        let request = write_request(&env, 60 + i as u8, operation());
        let (signed, binding) = sender.sign_request_to(request, &node(1)).unwrap();
        let admitted = receiver.verify_request(signed).unwrap();
        let response = receiver
            .sign_response(admitted.binding(), PeerResponse::Write(reply.clone()))
            .unwrap();
        let scope = admitted.request().origin.scope().clone();
        let (auth, _) = decode_envelope(
            encode_envelope(&response.authentication, true, 0).unwrap(),
            true,
            &peer_wire::test_util::AnyChain,
        )
        .unwrap();
        let decoded = codec.response(auth, Vec::new(), &scope).unwrap();
        match &decoded.response {
            PeerResponse::Write(decoded) => assert_eq!(decoded, &reply),
            _ => panic!("write reply expected for {reply:?}"),
        }
        assert_eq!(
            decoded.response.kind(),
            racer_core::read::ResponseKind::Write
        );
        let verified = sender.verify_response(decoded, &binding).unwrap();
        assert!(matches!(verified.response(), PeerResponse::Write(r) if r == &reply));
    }
}

#[test]
fn write_replies_that_do_not_answer_the_request_are_rejected() {
    let env = racer_io::Env::host();
    let signatures = network(&env, 2);
    let sender = Forwarding::new(signatures[0].clone());
    let receiver = Forwarding::new(signatures[1].clone());
    let base = request(&env, 1);
    let version = version(&base);
    let rejected = |operation: Operation, reply: WriteReply| {
        let request = if operation.is_write() {
            write_request(&env, 80, operation)
        } else {
            let mut request = request(&env, 80);
            request.operation = operation;
            request.route.destination = node(1);
            request
        };
        let (signed, binding) = sender.sign_request_to(request, &node(1)).unwrap();
        let admitted = receiver.verify_request(signed).unwrap();
        receiver
            .sign_response(admitted.binding(), PeerResponse::Write(reply))
            .ok()
            .is_none_or(|response| sender.verify_response(response, &binding).is_err())
    };
    let store_op = || Operation::WriteStore {
        version: version.clone(),
        store: Box::new(store(vec![0, 2])),
    };
    // Stored pages that were not requested.
    assert!(rejected(store_op(), WriteReply::Stored { pages: vec![1] }));
    assert!(rejected(
        store_op(),
        WriteReply::Stored { pages: vec![0, 3] }
    ));
    // `stored` answers only write-store, `uploaded` only write-upload.
    assert!(rejected(
        store_op(),
        WriteReply::Uploaded { pages: vec![0] }
    ));
    assert!(rejected(
        Operation::WriteUpload {
            version: version.clone(),
            session: SESSION,
            pages: vec![0],
        },
        WriteReply::Stored { pages: vec![0] }
    ));
    assert!(rejected(
        Operation::WriteRelease {
            version: version.clone(),
            session: SESSION,
            pages: vec![0],
        },
        WriteReply::Stored { pages: vec![0] }
    ));
    // A write reply never answers a read operation.
    assert!(rejected(
        Operation::Metadata {
            object: version.object.clone(),
            selector: MetadataSelector::Pinned(etag()),
            mode: FetchMode::CopyOnly,
        },
        WriteReply::Released
    ));
    // Replies that cannot be encoded.
    assert!(rejected(store_op(), WriteReply::Stored { pages: vec![] }));
    assert!(rejected(
        Operation::WriteSession {
            object: version.object.clone(),
            etag: None,
            action: Box::new(WriteAction::Status { wait_millis: 0 }),
        },
        WriteReply::Status {
            state: UploadState::Pending,
            etag: "not quoted".into(),
        }
    ));
    // Sanity: a matching reply is accepted.
    assert!(!rejected(store_op(), WriteReply::Stored { pages: vec![2] }));
}

#[test]
fn malformed_write_operations_are_rejected_at_encode() {
    let env = racer_io::Env::host();
    let object = request(&env, 1).origin.object;
    let version = ObjectVersion {
        object: object.clone(),
        etag: etag(),
    };
    let session = |etag: Option<StrongEtag>, action| Operation::WriteSession {
        object: object.clone(),
        etag,
        action,
    };
    let open = |spec| WriteAction::Open {
        session: SESSION,
        spec,
    };
    let mut bad = vec![
        // Required version missing.
        session(None, Box::new(open(spec()))),
        session(None, Box::new(WriteAction::Renew { session: SESSION })),
        session(None, Box::new(WriteAction::Abort { session: SESSION })),
        session(
            None,
            Box::new(WriteAction::Commit {
                session: SESSION,
                holders: holders(),
            }),
        ),
        session(
            None,
            Box::new(WriteAction::Replicate {
                manifest: manifest(),
            }),
        ),
        // Invalid specs.
        session(
            Some(etag()),
            Box::new(open(WriteSpec {
                replicas: 0,
                durability: Durability::Origin,
                ..spec()
            })),
        ),
        session(
            Some(etag()),
            Box::new(open(WriteSpec {
                replicas: 4,
                durability: Durability::Origin,
                ..spec()
            })),
        ),
        session(
            Some(etag()),
            Box::new(open(WriteSpec {
                durability: Durability::Replicas(3),
                ..spec()
            })),
        ),
        session(
            Some(etag()),
            Box::new(open(WriteSpec {
                length: 0,
                ..spec()
            })),
        ),
        session(
            Some(etag()),
            Box::new(open(WriteSpec {
                page_size: 3,
                ..spec()
            })),
        ),
        // A power of two the object protocol does not accept.
        session(
            Some(etag()),
            Box::new(open(WriteSpec {
                page_size: 2 * PAGE_BYTES,
                length: 1,
                ..spec()
            })),
        ),
        session(
            Some(etag()),
            Box::new(open(WriteSpec {
                content_type: Some("not a type".into()),
                ..spec()
            })),
        ),
        session(
            None,
            Box::new(WriteAction::Status {
                wait_millis: 30_001,
            }),
        ),
        session(
            Some(etag()),
            Box::new(WriteAction::Commit {
                session: SESSION,
                holders: Holders(vec![]),
            }),
        ),
        // A page without holders.
        session(
            Some(etag()),
            Box::new(WriteAction::Commit {
                session: SESSION,
                holders: Holders(vec![vec![node(1).0.into_bytes()], vec![]]),
            }),
        ),
        // The manifest names another version.
        session(
            Some(StrongEtag::parse(b"\"other\"").unwrap()),
            Box::new(WriteAction::Replicate {
                manifest: manifest(),
            }),
        ),
    ];
    for pages in [vec![], vec![1, 0], vec![1, 1], (0..257).collect()] {
        bad.push(Operation::WriteStore {
            version: version.clone(),
            store: Box::new(WriteStore {
                length: 300 * PAGE_BYTES,
                ..store(pages.clone())
            }),
        });
        bad.push(Operation::WriteRelease {
            version: version.clone(),
            session: SESSION,
            pages: pages.clone(),
        });
        bad.push(Operation::WriteUpload {
            version: version.clone(),
            session: SESSION,
            pages,
        });
    }
    // Out of range at the declared length.
    bad.push(Operation::WriteStore {
        version: version.clone(),
        store: Box::new(store(vec![4])),
    });
    for (i, operation) in bad.into_iter().enumerate() {
        let request = write_request(&env, 90, operation);
        assert!(
            request_head(&env.ctx.clock, &request).is_err(),
            "case {i} encoded"
        );
    }
    // Copy-only write operations never carry origin context.
    for operation in [
        Operation::WriteStore {
            version: version.clone(),
            store: Box::new(store(vec![0])),
        },
        Operation::WriteRelease {
            version: version.clone(),
            session: SESSION,
            pages: vec![0],
        },
    ] {
        let mut req = write_request(&env, 91, clone_operation(&operation));
        assert!(request_head(&env.ctx.clock, &req).is_ok());
        req.origin.authorization = request(&env, 91).origin.authorization;
        assert!(request_head(&env.ctx.clock, &req).is_err());
        let mut req = write_request(&env, 92, clone_operation(&operation));
        req.origin.metadata = request(&env, 92).origin.metadata;
        assert!(request_head(&env.ctx.clock, &req).is_err());
        // And never spend acquisition attempts.
        let mut req = write_request(&env, 93, operation);
        req.route.remaining_attempts = 1;
        assert!(request_head(&env.ctx.clock, &req).is_err());
    }
}

/// Mutate one header of a signed write request's proof and decode it.
fn tampered(
    env: &racer_io::Env,
    operation: Operation,
    change: &dyn Fn(&mut Vec<http1::Header>),
) -> (Result<()>, Result<()>) {
    let signatures = network(env, 2);
    let sender = Forwarding::new(signatures[0].clone());
    let receiver = Forwarding::new(signatures[1].clone());
    let (signed, _) = sender
        .sign_request_to(write_request(env, 100, operation), &node(1))
        .unwrap();
    let scope = signed.request.origin.scope().clone();
    let mut head = clone_head(&signed.authentication.original);
    change(&mut head.head.headers);
    let auth = ForwardedHead {
        original: Arc::new(head),
        hops: vec![],
    };
    let decoded = codec()
        .request(
            ForwardedHead {
                original: auth.original.clone(),
                hops: vec![],
            },
            &scope,
        )
        .map(drop);
    let verified = match decoded {
        Ok(()) => {
            let request = codec().request(auth, &scope).unwrap();
            receiver.verify_request(request).map(drop)
        }
        Err(_) => Ok(()),
    };
    (decoded, verified)
}

fn set(name: &'static str, value: &[u8]) -> impl Fn(&mut Vec<http1::Header>) + use<> {
    let value = value.to_vec();
    move |headers| {
        headers
            .iter_mut()
            .find(|h| h.name == name)
            .unwrap_or_else(|| panic!("{name} missing"))
            .value = value.clone();
    }
}

fn remove(name: &'static str) -> impl Fn(&mut Vec<http1::Header>) {
    move |headers| {
        let before = headers.len();
        headers.retain(|h| h.name != name);
        assert!(headers.len() < before, "{name} missing");
    }
}

fn duplicate(name: &'static str) -> impl Fn(&mut Vec<http1::Header>) {
    move |headers| {
        let header = headers.iter().find(|h| h.name == name).unwrap();
        let header = http1::Header {
            name: header.name.clone(),
            value: header.value.clone(),
        };
        headers.push(header);
    }
}

fn pages(list: &[u64]) -> Vec<u8> {
    crate::peer::protocol::binary(
        &list
            .iter()
            .flat_map(|p| p.to_be_bytes())
            .collect::<Vec<_>>(),
    )
    .into_bytes()
}

#[test]
fn malformed_write_operations_are_rejected_at_decode() {
    let env = racer_io::Env::host();
    let object = request(&env, 1).origin.object;
    let version = ObjectVersion {
        object: object.clone(),
        etag: etag(),
    };
    let open = || Operation::WriteSession {
        object: object.clone(),
        etag: Some(etag()),
        action: Box::new(WriteAction::Open {
            session: SESSION,
            spec: spec(),
        }),
    };
    let commit = || Operation::WriteSession {
        object: object.clone(),
        etag: Some(etag()),
        action: Box::new(WriteAction::Commit {
            session: SESSION,
            holders: holders(),
        }),
    };
    let status = |wait_millis| Operation::WriteSession {
        object: object.clone(),
        etag: None,
        action: Box::new(WriteAction::Status { wait_millis }),
    };
    let store_op = || Operation::WriteStore {
        version: version.clone(),
        store: Box::new(store(vec![0, 3])),
    };
    let release = || Operation::WriteRelease {
        version: version.clone(),
        session: SESSION,
        pages: vec![1],
    };
    let upload = || Operation::WriteUpload {
        version: version.clone(),
        session: SESSION,
        pages: vec![1],
    };
    let accepted = |operation: Operation, change: &dyn Fn(&mut Vec<http1::Header>)| {
        tampered(&env, operation, change).0.is_ok()
    };
    // Untouched requests decode.
    for operation in [open(), commit(), status(7), store_op(), release(), upload()] {
        assert!(accepted(operation, &|_| {}));
    }
    let three = format!("{}", 3 * PAGE_BYTES + 1).into_bytes();
    let short_session = crate::peer::protocol::binary(&[9; 15]).into_bytes();
    type Change = Box<dyn Fn(&mut Vec<http1::Header>)>;
    let bad: Vec<(Operation, Change)> = vec![
        (open(), Box::new(set(write::WRITE_REPLICAS, b"0"))),
        (open(), Box::new(set(write::WRITE_REPLICAS, b"4"))),
        (open(), Box::new(set(write::WRITE_REPLICAS, b"258"))),
        (
            open(),
            Box::new(set(write::WRITE_DURABILITY, b"replicas=3")),
        ),
        (open(), Box::new(set(write::WRITE_DURABILITY, b"disk"))),
        (open(), Box::new(set(write::WRITE_UPLOAD, b"Auto"))),
        (open(), Box::new(set(write::OBJECT_LENGTH, b"0"))),
        (open(), Box::new(set(write::PAGE_SIZE, b"12345"))),
        (open(), Box::new(set(write::WRITE_HOLD_UNTIL, b"-1"))),
        (open(), Box::new(set(write::WRITE_SESSION, &short_session))),
        (open(), Box::new(set(write::WRITE_ACTION, b"reopen"))),
        (open(), Box::new(set("racer-mode", b"copy"))),
        (open(), Box::new(remove(write::WRITE_SESSION))),
        (open(), Box::new(remove(write::WRITE_REPLICAS))),
        (open(), Box::new(remove(write::WRITE_HOLD_UNTIL))),
        (open(), Box::new(remove("racer-etag"))),
        (open(), Box::new(duplicate(write::WRITE_REPLICAS))),
        (open(), Box::new(duplicate(write::WRITE_ACTION))),
        (commit(), Box::new(set(write::WRITE_HOLDERS, b"AA=="))),
        (commit(), Box::new(remove(write::WRITE_HOLDERS))),
        (status(7), Box::new(set(write::WRITE_WAIT_MILLIS, b"0"))),
        (status(7), Box::new(set(write::WRITE_WAIT_MILLIS, b"30001"))),
        (status(7), Box::new(duplicate(write::WRITE_WAIT_MILLIS))),
        (store_op(), Box::new(set(write::WRITE_PAGES, &pages(&[4])))),
        (
            store_op(),
            Box::new(set(write::WRITE_PAGES, &pages(&[3, 0]))),
        ),
        (store_op(), Box::new(set(write::WRITE_PAGES, &pages(&[])))),
        (store_op(), Box::new(set(write::WRITE_PAGES, b"AAAA"))),
        (
            store_op(),
            Box::new(set(write::OBJECT_LENGTH, &three[..three.len() - 1])),
        ),
        (
            store_op(),
            Box::new(set(write::WRITE_SOURCE, b"not-a-node")),
        ),
        (store_op(), Box::new(set("racer-mode", b"acquire"))),
        (store_op(), Box::new(remove(write::WRITE_SOURCE))),
        (store_op(), Box::new(remove(write::WRITE_HOLD_UNTIL))),
        (store_op(), Box::new(duplicate(write::WRITE_PAGES))),
        (store_op(), Box::new(duplicate(write::CONTENT_TYPE))),
        (
            store_op(),
            Box::new(set(write::CONTENT_TYPE, b"not a type")),
        ),
        (release(), Box::new(set("racer-mode", b"acquire"))),
        (release(), Box::new(remove(write::WRITE_SESSION))),
        (release(), Box::new(remove("racer-etag"))),
        (upload(), Box::new(set("racer-mode", b"copy"))),
        (upload(), Box::new(remove(write::WRITE_PAGES))),
        (upload(), Box::new(set("racer-operation", b"write-delete"))),
    ];
    for (i, (operation, change)) in bad.into_iter().enumerate() {
        assert!(!accepted(operation, &*change), "case {i} accepted");
    }
}

/// Changing any write header after signing breaks the signature, even when
/// the changed request still decodes.
#[test]
fn signatures_bind_every_write_header() {
    let env = racer_io::Env::host();
    for operation in operations(&env) {
        let head = request_head(
            &env.ctx.clock,
            &write_request(&env, 100, clone_operation(&operation)),
        )
        .unwrap();
        let names: Vec<String> = head
            .headers
            .iter()
            .filter(|h| h.name.starts_with("racer-write-") || h.name == "racer-operation")
            .map(|h| h.name.clone())
            .collect();
        assert!(names.len() >= 2, "{names:?}");
        for name in names {
            let original = head.unique(&name).unwrap().unwrap().to_vec();
            // Flip one byte to another canonical-looking value.
            let mut changed = original.clone();
            let last = changed.len() - 1;
            changed[last] = match changed[last] {
                b'0'..=b'8' => changed[last] + 1,
                b'9' => b'8',
                b'A' => b'B',
                b'=' => {
                    let i = changed.iter().rposition(|b| *b != b'=').unwrap();
                    changed[i] = if changed[i] == b'A' { b'Q' } else { b'A' };
                    b'='
                }
                _ => b'A',
            };
            let leaked: &'static str = Box::leak(name.clone().into_boxed_str());
            let (decoded, verified) =
                tampered(&env, clone_operation(&operation), &set(leaked, &changed));
            assert!(
                decoded.is_err() || verified.is_err(),
                "{leaked}: {original:?} -> {changed:?} accepted"
            );
        }
    }
}
