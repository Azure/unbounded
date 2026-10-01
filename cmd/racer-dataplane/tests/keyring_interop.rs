//! Opt-in live Go handler interoperability. Run through TestRustKeyringInterop in
//! internal/racer, which owns the loopback server and ephemeral fixture directory.
//! Only readiness and trust-file reads use a test adapter; TLS, HTTP, enrollment,
//! and wire decoding are production implementations.
use racer_dataplane::{
    control::{ControlEndpoint, enrollment::Enrollment, transport::*, wire},
    error::{Error, Operation},
    model::{ClusterId, RequestId},
    runtime::{deadline::RequestScope, reactor::Descriptor},
};
use std::{
    os::fd::AsRawFd,
    path::Path,
    rc::Rc,
    time::{Duration, Instant},
};

struct Io;
impl ControlIo for Io {
    fn read_file<'a>(
        &'a self,
        path: &'a Path,
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
        Box::pin(async move {
            scope.check()?;
            let bytes = std::fs::read(path).map_err(|_| Error::Io)?;
            if bytes.len() > limit {
                return Err(Error::Overloaded);
            }
            Ok(zeroize::Zeroizing::new(bytes))
        })
    }
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        _: &'a RequestScope,
    ) -> Operation<'a, Vec<std::net::SocketAddr>> {
        Box::pin(async move {
            assert_eq!(host, "127.0.0.1");
            Ok(vec![std::net::SocketAddr::from(([127, 0, 0, 1], port))])
        })
    }
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            loop {
                scope.check()?;
                let mut poll = libc::pollfd {
                    fd: fd.as_raw_fd(),
                    events: (if read { libc::POLLIN } else { 0 })
                        | (if write { libc::POLLOUT } else { 0 }),
                    revents: 0,
                };
                let result = unsafe { libc::poll(&mut poll, 1, 10) };
                if result > 0 {
                    return Ok(());
                }
                if result < 0 {
                    return Err(Error::Io);
                }
            }
        })
    }
    fn sleep<'a>(&'a self, _: Instant, _: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async { Err(Error::InvalidConfiguration) })
    }
}

#[test]
#[ignore = "run RACER_RUST_INTEROP=1 go test ./internal/racer -run '^TestRustKeyringInterop$' -timeout=5m under the required external timeout"]
fn go_controller_keyring_bootstrap_mtls_and_rotation() {
    let directory = std::path::PathBuf::from(
        std::env::var_os("RACER_KEYRING_INTEROP_DIR").expect("Go fixture directory"),
    );
    let config_bytes =
        zeroize::Zeroizing::new(std::fs::read(directory.join("config.json")).unwrap());
    let mut config: std::collections::HashMap<String, String> =
        serde_json::from_slice(&config_bytes).unwrap();
    let token = zeroize::Zeroizing::new(config.remove("token").unwrap());
    let cluster = ClusterId(config["cluster"].clone());
    let enrollment = Enrollment::new(
        cluster.clone(),
        directory.join("unused-token"),
        directory.join("identity"),
    );
    let transport = ControlTransport::new(ControlEndpoint {
        url: config["endpoint"].clone(),
        trust_bundle: directory.join("trust.pem"),
    });
    transport.attach_io(Rc::new(Io));
    let scope =
        RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(90)).unwrap();

    futures::executor::block_on(async {
        for credential in [None, Some("")] {
            let response = transport
                .bootstrap(&scope)
                .await
                .unwrap()
                .request(
                    "GET",
                    wire::KEYRING_PATH,
                    credential,
                    &[],
                    wire::MAX_BUNDLE_BYTES,
                    &scope,
                )
                .await
                .unwrap();
            assert_eq!(response.status, 401);
            wire::decode_error(response.body.as_slice()).unwrap();
        }
        let response = transport
            .bootstrap(&scope)
            .await
            .unwrap()
            .request(
                "GET",
                wire::KEYRING_PATH,
                Some(&token),
                &[],
                wire::MAX_BUNDLE_BYTES,
                &scope,
            )
            .await
            .unwrap();
        assert_eq!(response.status, 200);
        let initial = wire::decode_bundle(response.body.as_slice()).unwrap();
        assert_eq!(initial.cluster, cluster);
        assert!(initial.generation.0 > 0);
        let cursor = format!("/v1/keyring?after={}", initial.generation.0);
        assert!(!initial.cache_keys.is_empty());
        enrollment
            .set_peer_trust_roots(initial.peer_trust_roots.clone())
            .unwrap();
        #[path = "support/enrollment.rs"]
        mod enrollment_io;
        let reactor = enrollment_io::reactor();
        enrollment.attach_reactor(reactor.clone());
        let request = enrollment_io::drive(&reactor, enrollment.prepare(&scope)).unwrap();
        let body = wire::encode_enrollment_request(&request).unwrap();
        let response = transport
            .bootstrap(&scope)
            .await
            .unwrap()
            .request(
                "POST",
                wire::BOOTSTRAP_PATH,
                Some(&token),
                &body,
                wire::MAX_ENROLLMENT_BYTES,
                &scope,
            )
            .await
            .unwrap();
        assert_eq!(response.status, 200);
        let identity = enrollment_io::drive(
            &reactor,
            enrollment.accept_response_async(
                wire::decode_enrollment_response(response.body.as_slice()).unwrap(),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(identity.node().0, config["node"]);

        let response = transport
            .authenticated(&identity, &scope)
            .await
            .unwrap()
            .request(
                "GET",
                wire::KEYRING_PATH,
                None,
                &[],
                wire::MAX_BUNDLE_BYTES,
                &scope,
            )
            .await
            .unwrap();
        assert_eq!(response.status, 200);
        assert!(
            wire::encode_bundle(&wire::decode_bundle(response.body.as_slice()).unwrap()).unwrap()
                == wire::encode_bundle(&initial).unwrap(),
            "bearer and mTLS bundles differ"
        );
        for (path, credential, status) in [
            ("/v1/keyring?after=999", None, 409),
            ("/v1/keyring?after=01", None, 400),
            (wire::KEYRING_PATH, Some(token.as_str()), 401),
        ] {
            let response = transport
                .authenticated(&identity, &scope)
                .await
                .unwrap()
                .request("GET", path, credential, &[], wire::MAX_BUNDLE_BYTES, &scope)
                .await
                .unwrap();
            assert_eq!(response.status, status);
            wire::decode_error(response.body.as_slice()).unwrap();
        }
        let started = Instant::now();
        let response = transport
            .authenticated(&identity, &scope)
            .await
            .unwrap()
            .request("GET", &cursor, None, &[], wire::MAX_BUNDLE_BYTES, &scope)
            .await
            .unwrap();
        assert_eq!(response.status, 204);
        assert!(response.body.is_empty());
        assert!(started.elapsed() >= Duration::from_secs(29));

        std::fs::write(directory.join("rotate"), []).unwrap();
        let response = transport
            .authenticated(&identity, &scope)
            .await
            .unwrap()
            .request("GET", &cursor, None, &[], wire::MAX_BUNDLE_BYTES, &scope)
            .await
            .unwrap();
        assert_eq!(response.status, 200);
        let rotated = wire::decode_bundle(response.body.as_slice()).unwrap();
        assert_eq!(rotated.cluster, cluster);
        assert_eq!(rotated.generation.0, initial.generation.0 + 1);
        assert!(
            wire::encode_bundle(&rotated).unwrap() != wire::encode_bundle(&initial).unwrap(),
            "rotation did not change the bundle"
        );

        let response = transport
            .authenticated(&identity, &scope)
            .await
            .unwrap()
            .request(
                "GET",
                wire::SNAPSHOT_PATH,
                None,
                &[],
                wire::MAX_PUBLICATION_BYTES,
                &scope,
            )
            .await
            .unwrap();
        assert_eq!(response.status, 200);
        let snapshot = wire::decode_publication(response.body.as_slice()).unwrap();
        assert_eq!(snapshot.cluster, cluster);
        assert!(
            snapshot
                .members
                .iter()
                .any(|member| member.node == *identity.node())
        );
    });
}
