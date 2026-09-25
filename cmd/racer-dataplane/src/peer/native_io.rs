//! Automatic native payload exchange on the same exclusive HTTP connection.
use super::{
    native::{self, Binding, Phase, extension},
    transfer::{Transfers, WireBuffer},
    wire::{PeerResponse, SignedRequest, SignedResponse, WireCodec},
};
use crate::{
    error::{Error, Result},
    http::pool::ConnectionLease,
    model::identity::NodeId,
    rdma::{
        permission::{AuthenticatedDescriptor, COMPLETION_HEADER, DESCRIPTOR_HEADER},
        session::{SETUP_BINDING_HEADER, SETUP_HEADER, SetupParameters},
    },
    runtime::{deadline::RequestScope, reactor::IoBuffer},
    security::{
        forwarding::ForwardedHead,
        signing::{SignedHead, VerifiedHead, signed_digest},
    },
    topology::rails::TransportPlan,
};
use std::time::{Duration, Instant};

fn recoverable(error: Error) -> bool {
    matches!(error, Error::Unavailable | Error::Io | Error::Overloaded)
}
fn native_scope(scope: &RequestScope) -> RequestScope {
    let mut bounded = scope.clone();
    bounded.deadline.0 = bounded
        .deadline
        .0
        .min(Instant::now() + Duration::from_secs(5));
    bounded
}
fn native_failure(error: Error, scope: &RequestScope) -> bool {
    scope.check().is_ok() && (recoverable(error) || error == Error::DeadlineExceeded)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        http::{
            codec::{Codec, MessageHead, StartLine},
            io::HttpIo,
            pool::HttpPool,
        },
        memory::pool::BufferPool,
        model::{
            envelope::{KeyId, Nonce, PageEnvelope},
            identity::*,
            limits::ResourceClass,
            metadata::{ExpiresAt, ObjectMetadata},
        },
        rdma::{
            device::Devices, permission::Permissions, registered::RegisteredPool,
            session::Sessions, transfer::RdmaTransfer, verbs::Verbs,
        },
        runtime::{admission::Admission, reactor::Reactor},
        security::{protocol as p, signing::Signatures},
    };
    use std::{
        os::unix::net::UnixStream,
        rc::Rc,
        sync::Arc,
        task::{Context, Poll},
        time::{Duration, Instant},
    };

    fn transfers(
        signatures: Rc<Signatures>,
        admission: &Rc<Admission>,
        reactor: &Rc<Reactor>,
    ) -> Transfers {
        let devices = Rc::new(Devices::new(Rc::new(Verbs)));
        let sessions = Rc::new(Sessions::new(devices.clone(), 2));
        let rdma = Rc::new(RdmaTransfer::new(
            sessions.clone(),
            Rc::new(RegisteredPool::new(devices, admission.clone())),
            Rc::new(Permissions),
        ));
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(
                super::super::wire::MAX_ENVELOPE_HEAD,
                crate::model::range::PAGE_BYTES + 16,
            ),
            admission.clone(),
        ));
        Transfers::new(
            Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
            io,
            Some(rdma),
        )
        .with_wire(
            admission.clone(),
            Rc::new(super::super::wire::SecurityCodec::new(
                admission.clone(),
                Rc::new(BufferPool::new(admission.clone())),
            )),
        )
        .with_native(signatures, sessions)
    }
    #[test]
    fn real_socket_signed_offer_falls_back_when_local_provider_is_unavailable() {
        fallback_roundtrip(false);
    }
    #[test]
    fn real_socket_sender_failure_requires_signed_fallback_before_ciphertext() {
        fallback_roundtrip(true);
    }
    fn fallback_roundtrip(sender_failure: bool) {
        offer_fallback(sender_failure);
    }
    #[test]
    fn native_subdeadline_preserves_parent_deadline_and_cancellation() {
        let scope = RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30))
            .unwrap();
        let native = native_scope(&scope);
        assert!(native.deadline.0 <= scope.deadline.0);
        assert!(native_failure(Error::DeadlineExceeded, &scope));
        assert!(!native_failure(Error::Unauthorized, &scope));
        scope.cancel().unwrap();
        assert_eq!(native.check(), Err(Error::Cancelled));
        assert!(!native_failure(Error::DeadlineExceeded, &scope));
    }
    fn offer_fallback(failed: bool) {
        let sender_failure = failed;
        let signers = super::super::tests::signers();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let receiver = transfers(signers[0].clone(), &admission, &reactor);
        let sender = transfers(signers[2].clone(), &admission, &reactor);
        let scope = RequestScope::new(RequestId([5; 16]), Instant::now() + Duration::from_secs(10))
            .unwrap();
        let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
        let version = ObjectVersion {
            object: ObjectId {
                cache: cache.clone(),
                key: CacheKey([3; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        };
        let envelope = PageEnvelope {
            page: PageId {
                version: version.clone(),
                number: PageNumber(0),
            },
            key_id: KeyId([1; 16]),
            nonce: Nonce([2; 24]),
            plaintext_length: 19,
            ciphertext_length: 35,
        };
        let page = BufferPool::new(admission.clone())
            .ciphertext(
                admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 35)
                    .unwrap(),
                envelope,
                vec![77; 35],
            )
            .unwrap();
        let response = PeerResponse::Page {
            metadata: ObjectMetadata {
                version,
                length: 19,
                expires_at: ExpiresAt(std::time::UNIX_EPOCH),
            },
            ciphertext: page,
        };
        let mut head = p::response_head(
            &response,
            &[4; 32],
            &[signers[0].node().clone(), signers[2].node().clone()],
        )
        .unwrap();
        p::push(&mut head, "racer-receiver", &signers[0].node().0);
        let authentication = ForwardedHead {
            original: Arc::new(signers[2].sign(head).unwrap()),
            hops: vec![],
        };
        let mut binding = Binding {
            request: [4; 32],
            response: [0; 32],
            transfer: TransferId([6; 16]),
            membership: 1,
            deadline: p::encode_deadline(scope.deadline).unwrap(),
            rail: crate::topology::rails::RailId(7),
        };
        let accept = binding
            .sign(
                &signers[0],
                signers[2].node(),
                Phase::Accept,
                &[0; 32],
                0,
                vec![],
            )
            .unwrap();
        binding.response = native::envelope_digest(&authentication).unwrap();
        // A well-formed signed remote endpoint cannot manufacture a local provider.
        let mut setup = b"racer-rdma-setup-v1\0".to_vec();
        setup.extend_from_slice(&7u16.to_be_bytes());
        setup.extend_from_slice(&[1; 16]);
        setup.extend_from_slice(&[2; 16]);
        for value in [1u32, 2, 3] {
            setup.extend_from_slice(&value.to_be_bytes());
        }
        setup.extend_from_slice(&1u16.to_be_bytes());
        setup.extend_from_slice(&[1, 1]);
        let offer = binding
            .sign(
                &signers[2],
                signers[0].node(),
                Phase::Offer,
                &signed_digest(&accept).unwrap(),
                0,
                vec![extension(SETUP_HEADER, p::binary(&setup).into_bytes())],
            )
            .unwrap();
        let previous = signed_digest(&offer).unwrap();
        let mut offered = WireCodec::encode(&authentication, true, 0).unwrap();
        native::attach(&mut offered, &offer).unwrap();
        let response = SignedResponse {
            authentication,
            response,
        };
        let (a, b) = UnixStream::pair().unwrap();
        let a = ConnectionLease::from_accepted(a.into(), &admission).unwrap();
        let b = ConnectionLease::from_accepted(b.into(), &admission).unwrap();
        let receive = async {
            let initial = MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/test".into(),
                },
                headers: vec![extension("content-length", b"0".to_vec())],
            };
            let sent = receiver.io.send_head(a, initial, &scope).await?;
            let mut received = receiver.io.receive_head(sent.connection, &scope).await?;
            let offer = native::detach(&mut received.value)?.unwrap();
            let (auth, _) = WireCodec::decode(received.value, true)?;
            let mut original = binding.clone();
            original.response = [0; 32];
            if sender_failure {
                let (verified, _) = binding.verify(
                    &signers[0],
                    signers[2].node(),
                    offer,
                    &[Phase::Offer],
                    &signed_digest(&accept)?,
                    0,
                    &scope,
                )?;
                let mut connection = received.connection;
                connection.finish_exchange()?;
                let setup = binding.sign(
                    &signers[0],
                    signers[2].node(),
                    Phase::Setup,
                    &signed_digest(&verified.signed)?,
                    0,
                    vec![
                        extension(SETUP_HEADER, p::binary(&[1; 32]).into_bytes()),
                        extension(SETUP_BINDING_HEADER, p::binary(&[2; 32]).into_bytes()),
                    ],
                )?;
                let setup_digest = signed_digest(&setup)?;
                connection = receiver.write_control(connection, setup, &scope).await?;
                let failed = receiver.read_control(connection, true, &scope).await?;
                let (mut connection, signed) = failed;
                let (failed, _) = binding.verify(
                    &signers[0],
                    signers[2].node(),
                    signed,
                    &[Phase::Failed],
                    &setup_digest,
                    0,
                    &scope,
                )?;
                connection.finish_exchange()?;
                return receiver
                    .receive_fallback(
                        connection,
                        auth,
                        &binding,
                        signers[2].node(),
                        signed_digest(&failed.signed)?,
                        &scope,
                    )
                    .await;
            }
            receiver
                .receive_native(
                    received.connection,
                    auth,
                    original,
                    accept,
                    signers[2].node().clone(),
                    offer,
                    &scope,
                )
                .await
        };
        let send = async {
            let received = sender.io.receive_head(b, &scope).await?;
            let sent = sender
                .io
                .send_head(received.connection, offered, &scope)
                .await?;
            let mut connection = sent.connection;
            connection.finish_exchange()?;
            if sender_failure {
                let (connection, setup) = sender.read_control(connection, false, &scope).await?;
                let (setup, _) = binding.verify(
                    &signers[2],
                    signers[0].node(),
                    setup,
                    &[Phase::Setup],
                    &previous,
                    0,
                    &scope,
                )?;
                let (mut connection, _) = sender
                    .failed_then_fallback(
                        connection,
                        &response,
                        &binding,
                        signers[0].node(),
                        signed_digest(&setup.signed)?,
                        &scope,
                    )
                    .await?;
                connection.finish_exchange()?;
                return Ok(());
            }
            let (connection, fallback) = sender.read_control(connection, false, &scope).await?;
            let (verified, phase) = binding.verify(
                &signers[2],
                signers[0].node(),
                fallback,
                &[Phase::Fallback],
                &previous,
                0,
                &scope,
            )?;
            assert_eq!(phase, Phase::Fallback);
            let (mut connection, sent) = sender
                .send_fallback(
                    connection,
                    &response,
                    &binding,
                    signers[0].node(),
                    signed_digest(&verified.signed)?,
                    &scope,
                )
                .await?;
            assert!(sent);
            connection.finish_exchange()?;
            Ok::<(), Error>(())
        };
        let mut future = std::pin::pin!(async { futures::try_join!(receive, send) });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let (result, ()) = loop {
            if let Poll::Ready(result) = std::future::Future::poll(future.as_mut(), &mut cx) {
                break result.unwrap();
            }
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        };
        assert_eq!(
            native::envelope_digest(&result.authentication).unwrap(),
            binding.response
        );
        match result.response {
            PeerResponse::Page { ciphertext, .. } => assert_eq!(ciphertext.bytes(), &[77; 35]),
            _ => panic!("expected retained ciphertext"),
        }
        assert_eq!(admission.used(ResourceClass::Registered), 0);
    }
    #[cfg(feature = "rdma")]
    #[test]
    #[ignore = "requires real ABI v2 adapter and RACER_RDMA_TEST_DEVICE/PORT/GID for an active type-2B port"]
    fn native_provider_signed_setup_grant_write_completion_roundtrip() {
        use crate::rdma::{
            device::FabricPort,
            lifecycle::{NativeService, pair},
        };
        use crate::topology::{
            membership::{Member, Membership},
            rails::{RailId, RailMapping},
        };
        let device = std::env::var("RACER_RDMA_TEST_DEVICE").expect("select real device");
        let port = std::env::var("RACER_RDMA_TEST_PORT")
            .expect("select port")
            .parse()
            .unwrap();
        let text = std::env::var("RACER_RDMA_TEST_GID").expect("32 lowercase hex GID digits");
        assert_eq!(text.len(), 32);
        let mut gid = [0; 16];
        for (i, b) in gid.iter_mut().enumerate() {
            *b = u8::from_str_radix(&text[2 * i..2 * i + 2], 16).unwrap();
        }
        let signers = super::super::tests::signers();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let mappings = vec![RailMapping {
            rail: RailId(7),
            fabric: "test-provider".into(),
            numa_node: None,
        }];
        let associations = vec![FabricPort {
            fabric: "test-provider".into(),
            device,
            port,
            gid: Some(gid),
        }];
        let scope = RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(30))
            .unwrap();
        let make = |signatures: Rc<Signatures>| {
            let (io, port) = pair(2).unwrap();
            let devices = Rc::new(Devices::new(Rc::new(Verbs)));
            devices.attach(io).unwrap();
            let sessions = Rc::new(Sessions::new(devices.clone(), 2));
            let rdma = Rc::new(RdmaTransfer::new(
                sessions.clone(),
                Rc::new(RegisteredPool::new(devices.clone(), admission.clone())),
                Rc::new(Permissions),
            ));
            let http = Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(
                    super::super::wire::MAX_ENVELOPE_HEAD,
                    crate::model::range::PAGE_BYTES + 16,
                ),
                admission.clone(),
            ));
            let transfer = Transfers::new(
                Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
                http,
                Some(rdma),
            )
            .with_wire(
                admission.clone(),
                Rc::new(super::super::wire::SecurityCodec::new(
                    admission.clone(),
                    Rc::new(BufferPool::new(admission.clone())),
                )),
            )
            .with_native(signatures, sessions);
            (devices, NativeService::new(port), transfer)
        };
        let (receive_devices, mut receive_engine, receiver) = make(signers[0].clone());
        let (send_devices, mut send_engine, sender) = make(signers[2].clone());
        let mut activate = std::pin::pin!(async {
            futures::try_join!(
                receive_devices.activate(
                    mappings.clone(),
                    associations.clone(),
                    &admission,
                    4096,
                    &scope
                ),
                send_devices.activate(
                    mappings.clone(),
                    associations.clone(),
                    &admission,
                    4096,
                    &scope
                )
            )
        });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if let Poll::Ready(result) = std::future::Future::poll(activate.as_mut(), &mut cx) {
                result.expect("native provider activation must succeed");
                break;
            }
            receive_engine.poll_budgeted(8).unwrap();
            send_engine.poll_budgeted(8).unwrap();
        }
        let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
        let version = ObjectVersion {
            object: ObjectId {
                cache: cache.clone(),
                key: CacheKey([9; 32]),
            },
            etag: StrongEtag::parse(b"\"provider\"").unwrap(),
        };
        let envelope = PageEnvelope {
            page: PageId {
                version: version.clone(),
                number: PageNumber(0),
            },
            key_id: KeyId([2; 16]),
            nonce: Nonce([3; 24]),
            plaintext_length: 128,
            ciphertext_length: 144,
        };
        let page = BufferPool::new(admission.clone())
            .ciphertext(
                admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 144)
                    .unwrap(),
                envelope,
                vec![0x5a; 144],
            )
            .unwrap();
        let response = PeerResponse::Page {
            metadata: ObjectMetadata {
                version,
                length: 128,
                expires_at: ExpiresAt(std::time::UNIX_EPOCH),
            },
            ciphertext: page,
        };
        let mut head = p::response_head(
            &response,
            &[4; 32],
            &[signers[0].node().clone(), signers[2].node().clone()],
        )
        .unwrap();
        p::push(&mut head, "racer-receiver", &signers[0].node().0);
        let response = SignedResponse {
            authentication: ForwardedHead {
                original: Arc::new(signers[2].sign(head).unwrap()),
                hops: vec![],
            },
            response,
        };
        let binding = Binding {
            request: [4; 32],
            response: [0; 32],
            transfer: TransferId([6; 16]),
            membership: 1,
            deadline: p::encode_deadline(scope.deadline).unwrap(),
            rail: RailId(7),
        };
        let accept = binding
            .sign(
                &signers[0],
                signers[2].node(),
                Phase::Accept,
                &[0; 32],
                0,
                vec![],
            )
            .unwrap();
        let accept_wire = super::super::wire::encode_signed(&accept).unwrap();
        let network = super::super::PeerNetwork::new(signers[2].node().clone(), 1).unwrap();
        network
            .install(Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    [0, 2]
                        .into_iter()
                        .map(|i| Member {
                            node: signers[i].node().clone(),
                            shares: std::num::NonZeroU32::new(1).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                            rails: mappings.clone(),
                            alignment_enabled: true,
                        })
                        .collect(),
                )
                .unwrap(),
            ))
            .unwrap();
        let (a, b) = UnixStream::pair().unwrap();
        let a = ConnectionLease::from_accepted(a.into(), &admission).unwrap();
        let b = ConnectionLease::from_accepted(b.into(), &admission).unwrap();
        let receive = async {
            let initial = MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/test".into(),
                },
                headers: vec![extension("content-length", b"0".to_vec())],
            };
            let sent = receiver.io.send_head(a, initial, &scope).await?;
            let mut offered = receiver.io.receive_head(sent.connection, &scope).await?;
            let control = native::detach(&mut offered.value)?.ok_or(Error::Unavailable)?;
            let (auth, length) = WireCodec::decode(offered.value, true)?;
            assert_eq!(length, 0, "provider test requires native offer");
            receiver
                .receive_native(
                    offered.connection,
                    auth,
                    binding.clone(),
                    accept,
                    signers[2].node().clone(),
                    control,
                    &scope,
                )
                .await
        };
        let send = async {
            let received = sender.io.receive_head(b, &scope).await?;
            let (verified, _) = binding.verify(
                &signers[2],
                signers[0].node(),
                super::super::wire::decode_signed(&accept_wire)?,
                &[Phase::Accept],
                &[0; 32],
                0,
                &scope,
            )?;
            let (mut conn, sent) = sender
                .send_native(
                    received.connection,
                    &response,
                    (binding.clone(), verified),
                    &network,
                    &scope,
                )
                .await?;
            assert!(sent);
            conn.finish_exchange()?;
            Ok::<(), Error>(())
        };
        let mut exchange = std::pin::pin!(async { futures::try_join!(receive, send) });
        let (result, ()) = loop {
            if let Poll::Ready(result) = std::future::Future::poll(exchange.as_mut(), &mut cx) {
                break result.expect("native provider roundtrip");
            }
            receive_engine.poll_budgeted(8).unwrap();
            send_engine.poll_budgeted(8).unwrap();
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        };
        match result.response {
            PeerResponse::Page { ciphertext, .. } => assert_eq!(ciphertext.bytes(), &[0x5a; 144]),
            _ => panic!("expected page"),
        }
        assert_eq!(
            sender.native_completed.get(),
            1,
            "HTTP fallback cannot pass a native success test"
        );
        assert_eq!(sender.native_fallbacks.get(), 0);
        assert_eq!(receiver.native_completions.get(), 1);
    }
}
async fn fence(session: &crate::rdma::session::SessionLease, scope: &RequestScope) -> Result<()> {
    let cancellation = scope.cancellation.subscribe()?;
    futures::future::poll_fn(|cx| {
        cancellation.register(cx.waker());
        session.abort()?;
        scope.check()?;
        session.qp.poll_stopped(cx)
    })
    .await
}

impl Transfers {
    pub(super) async fn send_native(
        &self,
        mut connection: ConnectionLease,
        response: &SignedResponse,
        admitted: (Binding, VerifiedHead),
        network: &super::PeerNetwork,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        let (signatures, sessions) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let Some(rdma) = &self.rdma else {
            return Ok((connection, false));
        };
        let PeerResponse::Page { ciphertext, .. } = &response.response else {
            return Ok((connection, false));
        };
        let (mut binding, accept) = admitted;
        let mut bounded_scope = scope.clone();
        bounded_scope.deadline.0 = bounded_scope
            .deadline
            .0
            .min(crate::security::protocol::decode_deadline(binding.deadline)?.0);
        let scope = &bounded_scope;
        scope.check()?;
        let peer = accept.peer.node();
        let path = crate::security::protocol::decode_nodes(
            crate::security::protocol::field(
                &response.authentication.original.head,
                "racer-response-path",
            )?
            .as_bytes(),
        )?;
        let route = crate::topology::paths::Route {
            membership: network.membership(crate::model::identity::MembershipVersion(
                binding.membership,
            ))?,
            nodes: path,
        };
        if crate::topology::rails::Rails.select(&route, &ciphertext.envelope().page)?
            != (TransportPlan::Rdma { rail: binding.rail })
            || !rdma.ready(binding.rail)
        {
            return Ok((connection, false));
        }
        let prepared = match sessions.prepare(&accept.peer, binding.rail) {
            Ok(p) => p,
            Err(e) if recoverable(e) => return Ok((connection, false)),
            Err(e) => return Err(e),
        };
        binding.response = native::envelope_digest(&response.authentication)?;
        let local_setup = prepared.setup().header_value();
        let offer = binding.sign(
            signatures,
            peer,
            Phase::Offer,
            &signed_digest(&accept.signed)?,
            0,
            vec![extension(SETUP_HEADER, local_setup.clone())],
        )?;
        let mut previous = signed_digest(&offer)?;
        let mut head = WireCodec::encode(&response.authentication, true, 0)?;
        native::attach(&mut head, &offer)?;
        connection = self.io.send_head(connection, head, scope).await?.connection;
        connection.finish_exchange()?;
        let (conn, setup) = self.read_control(connection, false, scope).await?;
        connection = conn;
        let (setup, phase) = binding.verify(
            signatures,
            peer,
            setup,
            &[Phase::Setup, Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&setup.signed)?;
        if phase == Phase::Fallback {
            drop(prepared);
            return self
                .send_fallback(connection, response, &binding, peer, previous, scope)
                .await;
        }
        let remote = SetupParameters::from_verified(&setup, binding.rail)?;
        let session = match prepared.finish(&setup) {
            Ok(session) => session,
            Err(error) if recoverable(error) => {
                return self
                    .failed_then_fallback(connection, response, &binding, peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let ready = binding.sign(
            signatures,
            peer,
            Phase::Ready,
            &previous,
            0,
            vec![
                extension(SETUP_HEADER, local_setup),
                extension(SETUP_BINDING_HEADER, remote.binding_header_value()),
            ],
        )?;
        previous = signed_digest(&ready)?;
        connection = self.write_control(connection, ready, scope).await?;
        connection.finish_exchange()?;
        let (conn, grant) = self.read_control(connection, false, scope).await?;
        connection = conn;
        let (grant, phase) = binding.verify(
            signatures,
            peer,
            grant,
            &[Phase::Grant, Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&grant.signed)?;
        if phase == Phase::Fallback {
            fence(&session, scope).await?;
            return self
                .send_fallback(connection, response, &binding, peer, previous, scope)
                .await;
        }
        let descriptor =
            AuthenticatedDescriptor::from_verified(&grant, &session, binding.transfer)?;
        let native_deadline = native_scope(scope);
        let complete = match rdma
            .send_to(&session, ciphertext.clone(), descriptor, &native_deadline)
            .await
        {
            Ok(complete) => complete,
            Err(error) if native_failure(error, scope) => {
                fence(&session, scope).await?;
                return self
                    .failed_then_fallback(connection, response, &binding, peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let complete = binding.sign(
            signatures,
            peer,
            Phase::Complete,
            &previous,
            0,
            vec![extension(COMPLETION_HEADER, complete.header_value())],
        )?;
        previous = signed_digest(&complete)?;
        connection = self.write_control(connection, complete, scope).await?;
        connection.finish_exchange()?;
        let (conn, done) = self.read_control(connection, false, scope).await?;
        connection = conn;
        let (done, phase) = binding.verify(
            signatures,
            peer,
            done,
            &[Phase::Done, Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&done.signed)?;
        if phase == Phase::Fallback {
            fence(&session, scope).await?;
            return self
                .send_fallback(connection, response, &binding, peer, previous, scope)
                .await;
        }
        let finish = binding.sign(signatures, peer, Phase::Finish, &previous, 0, vec![])?;
        connection = self.write_control(connection, finish, scope).await?;
        #[cfg(test)]
        self.native_completed.set(self.native_completed.get() + 1);
        Ok((connection, true))
    }
    async fn failed_then_fallback(
        &self,
        mut connection: ConnectionLease,
        response: &SignedResponse,
        binding: &Binding,
        peer: &NodeId,
        previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        let (signatures, _) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let failed = binding.sign(signatures, peer, Phase::Failed, &previous, 0, vec![])?;
        let previous = signed_digest(&failed)?;
        connection = self.write_control(connection, failed, scope).await?;
        connection.finish_exchange()?;
        let (connection, fallback) = self.read_control(connection, false, scope).await?;
        let (fallback, _) = binding.verify(
            signatures,
            peer,
            fallback,
            &[Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        self.send_fallback(
            connection,
            response,
            binding,
            peer,
            signed_digest(&fallback.signed)?,
            scope,
        )
        .await
    }
    async fn send_fallback(
        &self,
        connection: ConnectionLease,
        response: &SignedResponse,
        binding: &Binding,
        peer: &NodeId,
        previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        #[cfg(test)]
        self.native_fallbacks.set(self.native_fallbacks.get() + 1);
        scope.check()?;
        let (signatures, _) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let PeerResponse::Page { ciphertext, .. } = &response.response else {
            return Err(Error::InvalidRequest);
        };
        let finish = binding.sign(
            signatures,
            peer,
            Phase::Finish,
            &previous,
            ciphertext.bytes().len(),
            vec![],
        )?;
        let mut head = WireCodec::encode(&response.authentication, true, ciphertext.bytes().len())?;
        native::attach(&mut head, &finish)?;
        let connection = self.io.send_head(connection, head, scope).await?.connection;
        let (admission, _) = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
        let mut buffer = WireBuffer::new(admission, ciphertext.bytes().len())?;
        buffer.bytes_mut()?.copy_from_slice(ciphertext.bytes());
        let written = self.io.write_body(connection, buffer, scope).await?;
        Ok((written.lease, true))
    }
    pub(super) fn accept_native(
        &self,
        request: &SignedRequest,
        plan: TransportPlan,
        scope: &RequestScope,
    ) -> Result<Option<(Binding, SignedHead, NodeId)>> {
        let TransportPlan::Rdma { rail } = plan else {
            return Ok(None);
        };
        let Some((signatures, sessions)) = &self.native else {
            return Ok(None);
        };
        if !sessions.ready(rail) || !self.rdma.as_ref().is_some_and(|rdma| rdma.ready(rail)) {
            return Ok(None);
        }
        let peer = crate::security::signing::receiver(
            &request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original)
                .head,
        )?;
        let binding = Binding::request(
            &request.authentication,
            request.request.route.membership.0,
            scope,
            rail,
        )?;
        let accept = binding.sign(signatures, &peer, Phase::Accept, &[0; 32], 0, vec![])?;
        Ok(Some((binding, accept, peer)))
    }

    pub(super) fn admit_native(
        &self,
        request: &SignedRequest,
        control: SignedHead,
        scope: &RequestScope,
    ) -> Result<Option<(Binding, VerifiedHead)>> {
        let Some((signatures, _)) = &self.native else {
            return Ok(None);
        };
        let binding = Binding::parse_accept(&control)?;
        if binding.request != native::envelope_digest(&request.authentication)?
            || binding.response != [0; 32]
            || binding.membership != request.request.route.membership.0
            || binding.deadline
                > crate::security::protocol::encode_deadline(request.request.route.deadline)?
        {
            return Err(Error::Unauthorized);
        }
        let peer = crate::security::signing::node_field(
            &request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original)
                .head,
            "racer-signer",
        )?;
        let (verified, _) = binding.verify(
            signatures,
            &peer,
            control,
            &[Phase::Accept],
            &[0; 32],
            0,
            scope,
        )?;
        Ok(Some((binding, verified)))
    }

    async fn write_control(
        &self,
        connection: ConnectionLease,
        signed: SignedHead,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        Ok(self
            .io
            .send_head(connection, native::frame(signed)?, scope)
            .await?
            .connection)
    }
    async fn read_control(
        &self,
        connection: ConnectionLease,
        response: bool,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, SignedHead)> {
        let received = self.io.receive_head(connection, scope).await?;
        Ok((
            received.connection,
            native::unframe(received.value, response)?,
        ))
    }
    async fn read_ciphertext(
        &self,
        mut connection: ConnectionLease,
        length: usize,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, WireBuffer)> {
        let (admission, _) = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
        let mut buffer = WireBuffer::new(admission, length)?;
        let mut offset = 0;
        while offset < length {
            let read = self
                .io
                .read_body_range(connection, buffer, offset..length, scope)
                .await?;
            if read.bytes == 0 || read.bytes > length - offset {
                return Err(Error::Io);
            }
            offset += read.bytes;
            connection = read.lease;
            buffer = read.buffer;
        }
        Ok((connection, buffer))
    }

    /// A completed control round always pairs one request and one response before
    /// resetting HTTP framing. No pipelining or detached state map is required.
    pub(super) async fn receive_native(
        &self,
        mut connection: ConnectionLease,
        authentication: ForwardedHead,
        mut binding: Binding,
        accept: SignedHead,
        peer: NodeId,
        offer: SignedHead,
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        let (signatures, sessions) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let rdma = self.rdma.as_ref().ok_or(Error::Unavailable)?;
        let (admission, _) = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
        binding.response = native::envelope_digest(&authentication)?;
        let (offer, _) = binding.verify(
            signatures,
            &peer,
            offer,
            &[Phase::Offer],
            &signed_digest(&accept)?,
            0,
            scope,
        )?;
        let (metadata, envelope) = super::decode::page_descriptor(&authentication.original.head)?;
        let remote = SetupParameters::from_verified(&offer, binding.rail)?;
        let mut previous = signed_digest(&offer.signed)?;
        connection.finish_exchange()?;
        let prepared = match sessions.prepare(&offer.peer, binding.rail) {
            Ok(prepared) => prepared,
            Err(error) if recoverable(error) => {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let local_setup = prepared.setup().header_value();
        let setup = binding.sign(
            signatures,
            &peer,
            Phase::Setup,
            &previous,
            0,
            vec![
                extension(SETUP_HEADER, local_setup),
                extension(SETUP_BINDING_HEADER, remote.binding_header_value()),
            ],
        )?;
        previous = signed_digest(&setup)?;
        connection = self.write_control(connection, setup, scope).await?;
        let (conn, ready) = self.read_control(connection, true, scope).await?;
        connection = conn;
        let (ready, phase) = binding.verify(
            signatures,
            &peer,
            ready,
            &[Phase::Ready, Phase::Failed],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&ready.signed)?;
        connection.finish_exchange()?;
        if phase == Phase::Failed {
            drop(prepared);
            return self
                .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                .await;
        }
        if SetupParameters::from_verified(&ready, binding.rail)?.encoded != remote.encoded {
            return Err(Error::Unauthorized);
        }
        let session = match prepared.finish(&ready) {
            Ok(session) => session,
            Err(error) if recoverable(error) => {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let native_deadline = native_scope(scope);
        if let Err(error) = session.wait_ready(&native_deadline).await {
            fence(&session, scope).await?;
            if native_failure(error, scope) {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            return Err(error);
        }
        let grant = match rdma.prepare_receive(&session, &envelope, binding.transfer, scope) {
            Ok(grant) => grant,
            Err(error) if recoverable(error) => {
                fence(&session, scope).await?;
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        if let Err(error) = grant.wait_bound(&native_deadline).await {
            fence(&session, scope).await?;
            drop(grant);
            if native_failure(error, scope) {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            return Err(error);
        }
        let request = binding.sign(
            signatures,
            &peer,
            Phase::Grant,
            &previous,
            0,
            vec![extension(DESCRIPTOR_HEADER, grant.header_value()?)],
        )?;
        previous = signed_digest(&request)?;
        connection = self.write_control(connection, request, scope).await?;
        let (conn, completed) = self.read_control(connection, true, scope).await?;
        connection = conn;
        let (completed, phase) = binding.verify(
            signatures,
            &peer,
            completed,
            &[Phase::Complete, Phase::Failed],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&completed.signed)?;
        connection.finish_exchange()?;
        if phase == Phase::Failed {
            fence(&session, scope).await?;
            drop(grant);
            return self
                .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                .await;
        }
        let native_deadline = native_scope(scope);
        let page = match rdma
            .finish_receive(
                &session,
                grant,
                &completed,
                envelope,
                admission,
                &native_deadline,
            )
            .await
        {
            Ok(page) => page,
            Err(error) if native_failure(error, scope) => {
                fence(&session, scope).await?;
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        #[cfg(test)]
        self.native_completions
            .set(self.native_completions.get() + 1);
        let done = binding.sign(signatures, &peer, Phase::Done, &previous, 0, vec![])?;
        previous = signed_digest(&done)?;
        connection = self.write_control(connection, done, scope).await?;
        let (mut conn, finish) = self.read_control(connection, true, scope).await?;
        binding.verify(
            signatures,
            &peer,
            finish,
            &[Phase::Finish],
            &previous,
            0,
            scope,
        )?;
        conn.finish_exchange()?;
        // The original envelope is verified by the requester/relay's outstanding
        // binding after this transport returns. No plaintext is published here.
        let response = PeerResponse::Page {
            metadata,
            ciphertext: page,
        };
        let original = &authentication.original.head;
        let request_digest = crate::security::protocol::decode_binary(
            crate::security::protocol::field(original, "racer-request-binding")?.as_bytes(),
        )?
        .try_into()
        .map_err(|_| Error::InvalidRequest)?;
        let path = crate::security::protocol::decode_nodes(
            crate::security::protocol::field(original, "racer-response-path")?.as_bytes(),
        )?;
        crate::security::protocol::agrees(
            original,
            &crate::security::protocol::response_head(&response, &request_digest, &path)?,
            false,
        )?;
        Ok(SignedResponse {
            authentication,
            response,
        })
    }

    async fn receive_fallback(
        &self,
        connection: ConnectionLease,
        authentication: ForwardedHead,
        binding: &Binding,
        peer: &NodeId,
        previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        scope.check()?;
        let (signatures, _) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let fallback = binding.sign(signatures, peer, Phase::Fallback, &previous, 0, vec![])?;
        let previous = signed_digest(&fallback)?;
        let connection = self.write_control(connection, fallback, scope).await?;
        let mut received = self.io.receive_head(connection, scope).await?;
        let control = native::detach(&mut received.value)?.ok_or(Error::Unauthorized)?;
        let (returned, length) = WireCodec::decode(received.value, true)?;
        if native::envelope_digest(&returned)? != binding.response {
            return Err(Error::Unauthorized);
        }
        binding.verify(
            signatures,
            peer,
            control,
            &[Phase::Finish],
            &previous,
            length,
            scope,
        )?;
        let (mut connection, buffer) = self
            .read_ciphertext(received.connection, length, scope)
            .await?;
        let (bytes, _reservation) = buffer.into_parts();
        let (_, codec) = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
        let response = codec.response(authentication, bytes, scope)?;
        connection.finish_exchange()?;
        Ok(response)
    }
}
