//! Standalone public-API smoke tests, runnable against the production library
//! while unrelated unit tests in a shared worktree are being integrated.
use racer_dataplane::control::*;
pub use racer_dataplane::{error, model, runtime, security, topology};
#[path = "testing.rs"]
mod testing;

#[test]
fn public_codecs_and_enrollment_recovery() {
    let publication =
        wire::decode_publication(include_bytes!("testdata/publication.json")).unwrap();
    let bytes = wire::encode_publication(&publication).unwrap();
    use sha2::{Digest, Sha256};
    let (content, membership) = wire::canonical_content(&publication).unwrap();
    assert_eq!(
        format!("{:x}", Sha256::digest(content)),
        "9c9791a4a7c4863990f46383126839d3779c5299e5f4426a9487263c2af2e03b"
    );
    assert_eq!(
        format!("{:x}", Sha256::digest(membership)),
        "70bcaf18d9a87f3cc72eef79e3163c02c285c186d7811229e5bc2ccd94336617"
    );
    assert_eq!(
        wire::encode_publication(&wire::decode_publication(&bytes).unwrap()).unwrap(),
        bytes
    );
    let bundle = wire::decode_bundle(include_bytes!("testdata/bundle.json")).unwrap();
    assert_eq!(
        wire::encode_bundle(&bundle).unwrap(),
        include_str!("testdata/bundle.json").trim().as_bytes()
    );
    let request =
        wire::decode_enrollment_request(include_bytes!("testdata/bootstrap-request.json")).unwrap();
    assert_eq!(
        wire::encode_enrollment_request(&request).unwrap(),
        include_str!("testdata/bootstrap-request.json")
            .trim()
            .as_bytes()
    );
    let response =
        wire::decode_enrollment_response(include_bytes!("testdata/bootstrap-response.json"))
            .unwrap();
    assert_eq!(
        wire::encode_enrollment_response(&response).unwrap(),
        include_str!("testdata/bootstrap-response.json")
            .trim()
            .as_bytes()
    );
    let d = testing::Directory::new();
    let e =
        enrollment::Enrollment::new(publication.cluster, d.0.join("token"), d.0.join("identity"));
    let pending = e.prepare_now().unwrap();
    assert_eq!(pending.csr_der, e.prepare_now().unwrap().csr_der);
    let (ca, key) = testing::ca();
    e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
    let response = testing::issue(&pending, &ca, &key, "22222222-2222-4222-8222-222222222222");
    let identity = e.accept_response(response).unwrap();
    assert!(identity.valid_now());
    assert_eq!(e.load_identity().unwrap().unwrap().node(), identity.node());
}

#[test]
fn go_rejection_vectors_when_available() {
    let directory = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../racer-control-plane-implementation/internal/racer/wire/testdata");
    let mutations: serde_json::Value =
        serde_json::from_slice(&std::fs::read(directory.join("rejections.json")).unwrap()).unwrap();
    for case in mutations.as_array().unwrap() {
        let file = case["file"].as_str().unwrap();
        let original = std::fs::read_to_string(directory.join(file)).unwrap();
        let changed = original.replacen(
            case["old"].as_str().unwrap(),
            case["new"].as_str().unwrap(),
            1,
        );
        assert_ne!(original, changed);
        let result = match file {
            "publication.json" => wire::decode_publication(changed.as_bytes()).map(|_| ()),
            "bundle.json" => wire::decode_bundle(changed.as_bytes()).map(|_| ()),
            "bootstrap-request.json" => {
                wire::decode_enrollment_request(changed.as_bytes()).map(|_| ())
            }
            "bootstrap-response.json" => {
                wire::decode_enrollment_response(changed.as_bytes()).map(|_| ())
            }
            _ => panic!("unknown fixture"),
        };
        assert!(result.is_err(), "{}", case["name"]);
    }
}

struct Io;
impl transport::ControlIo for Io {
    fn resolve<'a>(
        &'a self,
        _: &'a str,
        port: u16,
        _: &'a runtime::deadline::RequestScope,
    ) -> error::Operation<'a, Vec<std::net::SocketAddr>> {
        Box::pin(async move { Ok(vec![std::net::SocketAddr::from(([127, 0, 0, 1], port))]) })
    }
    fn ready<'a>(
        &'a self,
        fd: std::rc::Rc<std::os::fd::OwnedFd>,
        read: bool,
        write: bool,
        scope: &'a runtime::deadline::RequestScope,
    ) -> error::Operation<'a, ()> {
        use std::os::fd::AsRawFd;
        Box::pin(async move {
            loop {
                scope.check()?;
                let mut poll = libc::pollfd {
                    fd: fd.as_raw_fd(),
                    events: (if read { libc::POLLIN } else { 0 })
                        | (if write { libc::POLLOUT } else { 0 }),
                    revents: 0,
                };
                let n = unsafe { libc::poll(&mut poll, 1, 10) };
                if n > 0 {
                    return Ok(());
                }
                if n < 0 {
                    return Err(error::Error::Io);
                }
            }
        })
    }
    fn sleep<'a>(
        &'a self,
        _: std::time::Instant,
        _: &'a runtime::deadline::RequestScope,
    ) -> error::Operation<'a, ()> {
        Box::pin(async { Ok(()) })
    }
}
#[test]
fn production_transport_server_auth_and_mtls() {
    use std::{
        io::{Read, Write},
        rc::Rc,
        sync::Arc,
        time::Duration,
    };
    let d = testing::Directory::new();
    let (ca, key) = testing::ca();
    let server_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let mut params = rcgen::CertificateParams::new(vec!["localhost".into()]).unwrap();
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    let cert = params.signed_by(&server_key, &ca, &key).unwrap();
    let trust = d.0.join("trust.pem");
    std::fs::write(&trust, ca.pem()).unwrap();
    let enrollment = enrollment::Enrollment::new(
        model::identity::ClusterId("11111111-1111-4111-8111-111111111111".into()),
        d.0.join("token"),
        d.0.join("identity"),
    );
    enrollment
        .set_peer_trust_roots(vec![ca.der().to_vec()])
        .unwrap();
    let pending = enrollment.prepare_now().unwrap();
    let identity = enrollment
        .accept_response(testing::issue(
            &pending,
            &ca,
            &key,
            "22222222-2222-4222-8222-222222222222",
        ))
        .unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.der().clone()).unwrap();
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        provider.clone(),
    )
    .allow_unauthenticated()
    .build()
    .unwrap();
    let config = rustls::ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_client_cert_verifier(verifier)
        .with_single_cert(
            vec![cert.der().clone()],
            rustls::pki_types::PrivatePkcs8KeyDer::from(server_key.serialize_der()).into(),
        )
        .unwrap();
    let server = std::thread::spawn(move || {
        for mutual in [false, true] {
            let (socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            let mut stream = rustls::StreamOwned::new(
                rustls::ServerConnection::new(Arc::new(config.clone())).unwrap(),
                socket,
            );
            let mut request = Vec::new();
            let mut byte = [0; 1];
            while !request.ends_with(b"\r\n\r\n") {
                stream.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            assert_eq!(stream.conn.peer_certificates().is_some(), mutual);
            stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n").unwrap();
            stream.flush().unwrap();
        }
    });
    let t = transport::ControlTransport::new(client::ControlEndpoint {
        url: format!("https://localhost:{port}"),
        trust_bundle: trust,
    });
    t.attach_io(Rc::new(Io));
    let scope = testing::scope();
    let response = futures::executor::block_on(async {
        t.bootstrap(&scope)
            .await?
            .request(
                "POST",
                wire::BOOTSTRAP_PATH,
                Some("fixture.token"),
                &[],
                65536,
                &scope,
            )
            .await
    })
    .unwrap();
    assert_eq!(response.body, b"{}");
    let response = futures::executor::block_on(async {
        t.authenticated(&identity, &scope)
            .await?
            .request("GET", wire::SNAPSHOT_PATH, None, &[], 65536, &scope)
            .await
    })
    .unwrap();
    assert_eq!(response.body, b"{}");
    server.join().unwrap();
}

#[test]
fn production_snapshot_lease_caps_and_projection_rejection() {
    use std::{os::unix::fs::symlink, rc::Rc, sync::Arc};
    let mut p = wire::decode_publication(include_bytes!("testdata/publication.json")).unwrap();
    p.sequence.0 = 1;
    p.membership_version.0 = 1;
    for m in &mut p.members {
        for r in &mut m.rails {
            r.fabric = "fabric-a".into();
        }
    }
    let store =
        snapshot::SnapshotStore::new(p.cluster.clone(), Arc::new(snapshot::PublishedState), 1);
    let first = store.publish(p.clone()).unwrap();
    assert!(Arc::ptr_eq(&first, &store.publish(p.clone()).unwrap()));
    p.sequence.0 = 2;
    let second = store.publish(p.clone()).unwrap();
    p.sequence.0 = 3;
    assert!(matches!(
        store.publish(p.clone()),
        Err(error::Error::Overloaded)
    ));
    drop(first);
    assert!(store.publish(p.clone()).is_ok());
    drop(second);
    let registry = caches::CacheRegistry;
    assert!(matches!(
        registry.reconcile(&p.caches).unwrap().as_slice(),
        [caches::CacheEvent::Add(_)]
    ));
    let old = p.caches[0].id.clone();
    p.caches[0].id.0 = "66666666-6666-4666-8666-666666666666".into();
    assert!(
        matches!(&registry.reconcile(&p.caches).unwrap()[0],caches::CacheEvent::Remove(id) if *id == old)
    );
    let d = testing::Directory::new();
    let (ca, _) = testing::ca();
    let keys = Rc::new(security::keyring::Keyring::new(
        p.cluster,
        p.members[0].node.clone(),
        Arc::new(security::keyring::KeyEpochs::default()),
    ));
    let watcher = secrets::SecretWatcher::new(d.0.clone(), keys.clone());
    let mut bundle = wire::decode_bundle(include_bytes!("testdata/bundle.json")).unwrap();
    bundle.generation.0 = 1;
    bundle.peer_trust_roots = vec![ca.der().to_vec()];
    // The public Go fixture repeats test material across purposes; production
    // security intentionally rejects such material reuse. Keep the page epochs.
    bundle.cache_keys.truncate(2);
    std::fs::create_dir(d.0.join("epoch")).unwrap();
    std::fs::write(
        d.0.join("epoch/bundle.json"),
        wire::encode_bundle(&bundle).unwrap(),
    )
    .unwrap();
    symlink("epoch", d.0.join("..data")).unwrap();
    assert_eq!(watcher.reload_now().unwrap().0, wire::BundleGeneration(1));
    assert_eq!(watcher.reload_now().unwrap().0, wire::BundleGeneration(1));
    std::fs::write(d.0.join("epoch/bundle.json"), b"{}").unwrap();
    assert!(watcher.reload_now().is_err());
    assert!(
        keys.active(&old, security::keyring::KeyPurpose::Page)
            .is_ok()
    );
    std::fs::remove_file(d.0.join("..data")).unwrap();
    symlink("../", d.0.join("..data")).unwrap();
    assert!(watcher.read_bundle().is_err());
}
