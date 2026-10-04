//! Opt-in live Go handler interoperability. Run through TestRustKeyringInterop in
//! internal/racer, which owns the loopback server and ephemeral fixture directory.
//! Readiness, trust-file reads, TLS, HTTP, enrollment, and wire decoding use
//! production implementations driven by the fixture's real reactor.
use racer_control_wire as wire;
use racer_dataplane as dataplane;
#[path = "support/enrollment.rs"]
#[allow(dead_code)]
mod enrollment_io;
use racer_control_wire as state;
use racer_dataplane::control::ControlEndpoint;
use racer_dataplane::control::Enrollment;
use racer_dataplane::control::*;
use racer_dataplane::model::ClusterId;
use racer_dataplane::model::RequestId;
use racer_dataplane::runtime::RequestScope;
use std::rc::Rc;
use std::time::Duration;
use std::time::Instant;

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
    assert!(config["endpoint"].starts_with("https://127.0.0.1:"));
    let reactor = enrollment_io::reactor();
    transport.attach_io(Rc::new(ReactorControlIo::new(reactor.clone())));
    let scope =
        RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(90)).unwrap();

    enrollment_io::drive_until(
        &reactor,
        async {
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
            enrollment.attach_reactor(reactor.clone());
            let request = enrollment.prepare(&scope).await.unwrap();
            let body = state::encode_enrollment_request(&request).unwrap();
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
            let identity = enrollment
                .accept_response_async(
                    wire::decode_enrollment_response(response.body.as_slice()).unwrap(),
                    &scope,
                )
                .await
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
                wire::encode_bundle(&wire::decode_bundle(response.body.as_slice()).unwrap())
                    .unwrap()
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
            Ok(())
        },
        scope.deadline.0,
    )
    .unwrap();
}
