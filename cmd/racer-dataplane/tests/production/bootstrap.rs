//! Atomic bootstrap remains a peer operation, not a v2 client subscription.
//! Enter at the real verified local peer service boundary; socket/session transport
//! is not part of this helper. Signatures, credential opening, origin HTTP,
//! acquisition, encryption, and persistence are production implementations.
use super::*;
use racer_dataplane::{
    model::{Authorization, OpaqueMetadata, OriginContext},
    peer::{
        server::LocalPageService,
        wire::{FetchMode, Operation, PeerRequest, PeerResponse},
    },
    security::identity::PendingIdentity,
    topology::{membership::MembershipLease, paths::RouteBudget},
};

const REQUESTER: &str = "33333333-3333-4333-8333-333333333333";

pub(super) struct Bootstrap {
    sender: Forwarding,
    receiver: Rc<Forwarding>,
    credentials: CredentialCrypto,
    coordinator: Rc<Coordinator>,
    membership: MembershipLease,
}

pub(super) fn identities(bundle: &mut serde_json::Value, keys: &Keyring) -> Rc<Keyring> {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = params.self_signed(&ca_key).unwrap();
    let roots = vec![ca.der().to_vec()];
    bundle["peer_trust_roots"] =
        serde_json::json!([base64::engine::general_purpose::STANDARD.encode(&roots[0])]);
    let sender = Rc::new(Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(REQUESTER.into()),
        Arc::new(KeyEpochs::default()),
    ));
    for keyring in [keys, sender.as_ref()] {
        keyring
            .install(wire::decode_bundle(&serde_json::to_vec(bundle).unwrap()).unwrap())
            .unwrap();
        let pending = PendingIdentity::generate().unwrap();
        let secret = pending.export_pkcs8_for_persistence().unwrap();
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(secret.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{CLUSTER}/node/{}", keyring.node().0)
                .try_into()
                .unwrap(),
        )];
        params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        let certificate = params.signed_by(&key, &ca, &ca_key).unwrap();
        keyring
            .install_signing_identity(Arc::new(
                pending
                    .accept(
                        ClusterId(CLUSTER.into()),
                        keyring.node().clone(),
                        vec![certificate.der().to_vec()],
                        &roots,
                    )
                    .unwrap(),
            ))
            .unwrap();
    }
    sender
}

impl Bootstrap {
    pub(super) fn new(
        sender: Rc<Keyring>,
        receiver: Rc<Forwarding>,
        admission: Rc<Admission>,
        coordinator: Rc<Coordinator>,
        membership: MembershipLease,
    ) -> Self {
        let certificates = Rc::new(Certificates::new(ClusterId(CLUSTER.into()), sender.clone()));
        Self {
            sender: Forwarding::new(Rc::new(Signatures::new(sender.clone(), certificates))),
            receiver,
            credentials: CredentialCrypto::new(sender, admission),
            coordinator,
            membership,
        }
    }

    pub(super) async fn acquire(&self, key: u8, scope: &RequestScope) -> Result<Reply> {
        let context = OriginContext {
            object: ObjectId {
                cache: CacheId(CACHE.into()),
                key: CacheKey([key; 32]),
            },
            metadata: Some(OpaqueMetadata::from_header(b"fixture-metadata")?),
            authorization: Some(Authorization::from_header(b"fixture-credential")?),
        };
        let attempt = AttemptId(scope.request.0);
        let request = PeerRequest {
            operation: Operation::Bootstrap {
                object: context.object.clone(),
                mode: FetchMode::Acquire,
            },
            origin: self.credentials.seal(&context, attempt, scope)?,
            route: RouteBudget {
                membership: self.membership.version,
                request: scope.request,
                attempt,
                destination: NodeId(NODE.into()),
                visited: vec![NodeId(REQUESTER.into())],
                remaining_links: 4,
                remaining_attempts: 8,
                deadline: scope.deadline,
            },
        };
        let (signed, binding) = self.sender.sign_request(request)?;
        let verified = self.receiver.verify_request(signed)?;
        let response_binding = verified.binding().clone();
        let response = self
            .coordinator
            .serve_peer(verified, self.membership.clone(), scope)
            .await?;
        let verified = self.sender.verify_response(
            self.receiver.sign_response(&response_binding, response)?,
            &binding,
        )?;
        match verified.response() {
            PeerResponse::Bootstrap {
                metadata,
                page_zero,
            } => {
                let mut body = Vec::new();
                if let Some(page) = page_zero {
                    use chacha20poly1305::{KeyInit, XChaCha20Poly1305, aead::AeadInOut};
                    metadata.immutable().validate_page(page.envelope())?;
                    assert_eq!(page.envelope().page.number, PageNumber(0));
                    body = page.bytes().to_vec();
                    XChaCha20Poly1305::new((&[7; 32]).into())
                        .decrypt_in_place(
                            (&page.envelope().nonce.0).into(),
                            &racer_dataplane::security::aead::page_aad(page.envelope())?,
                            &mut body,
                        )
                        .expect("authenticate actual bootstrap ciphertext");
                } else {
                    assert_eq!(metadata.length, 0);
                }
                assert_eq!(body.len() as u64, metadata.length.min(P));
                // Normalize only the assertion view shared with client scenarios.
                // These fields are not a fabricated HTTP response or read result.
                Ok(Reply {
                    status: 200,
                    fields: BTreeMap::from([
                        ("etag".into(), metadata.version.etag.as_str().into()),
                        ("racer-object-length".into(), metadata.length.to_string()),
                        ("racer-range-start".into(), "0".into()),
                        ("racer-range-end".into(), body.len().to_string()),
                        (
                            "racer-expires-at".into(),
                            metadata
                                .expires_at
                                .0
                                .duration_since(UNIX_EPOCH)
                                .unwrap()
                                .as_millis()
                                .to_string(),
                        ),
                    ]),
                    body,
                })
            }
            PeerResponse::Unavailable | PeerResponse::Overloaded => Ok(Reply {
                status: 503,
                fields: BTreeMap::new(),
                body: vec![],
            }),
            _ => panic!("unexpected bootstrap response"),
        }
    }
}

impl Rig {
    pub(super) fn bootstrap(&self, key: u8) -> Reply {
        self.drive(self.bootstrap.acquire(key, &scope())).unwrap()
    }
}
