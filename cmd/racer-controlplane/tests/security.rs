// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Standalone inclusion lets these tests run while the coordinator wires lib.rs.
#[path = "../src/security.rs"]
#[allow(dead_code)]
mod security;

use anyhow::{Result, ensure};
use security::*;
use std::sync::{Arc, Mutex};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
};

#[derive(Clone, Copy, Default)]
enum Fault {
    #[default]
    None,
    Before,
    After,
    Readback,
    PublicationAfter,
    PublicationBefore,
}

#[derive(Default)]
struct Memory {
    snapshot: StoreSnapshot,
    revision: u64,
    fault: Fault,
    fail_read: bool,
}

#[derive(Clone, Default)]
struct Store(Arc<Mutex<Memory>>);

impl Store {
    fn fault(&self, fault: Fault) {
        self.0.lock().unwrap().fault = fault;
    }
}

impl CaStore for Store {
    async fn read(&self) -> Result<StoreSnapshot> {
        let mut memory = self.0.lock().unwrap();
        if memory.fail_read {
            memory.fail_read = false;
            anyhow::bail!("injected readback failure");
        }
        Ok(memory.snapshot.clone())
    }

    async fn commit(&self, expected: &StoreSnapshot, next: &StateImage) -> Result<CommitOutcome> {
        let mut memory = self.0.lock().unwrap();
        if expected.resource_version != memory.snapshot.resource_version {
            return Ok(CommitOutcome::Conflict);
        }
        let fault = std::mem::take(&mut memory.fault);
        if matches!(fault, Fault::Before) {
            return Ok(CommitOutcome::Uncertain);
        }
        memory.revision += 1;
        memory.snapshot.image = Some(next.clone());
        memory.snapshot.resource_version = Some(memory.revision.to_string());
        memory.fail_read = matches!(fault, Fault::Readback);
        Ok(if matches!(fault, Fault::After | Fault::Readback) {
            CommitOutcome::Uncertain
        } else {
            CommitOutcome::Committed
        })
    }

    async fn publish(
        &self,
        expected: &StoreSnapshot,
        fence: &str,
        bytes: &[u8],
    ) -> Result<CommitOutcome> {
        let mut memory = self.0.lock().unwrap();
        if expected.resource_version != memory.snapshot.resource_version
            || expected.publication.as_ref().map(|p| &p.resource_version)
                != memory
                    .snapshot
                    .publication
                    .as_ref()
                    .map(|p| &p.resource_version)
        {
            return Ok(CommitOutcome::Conflict);
        }
        let state = CaState::from_image(memory.snapshot.image.as_ref().unwrap())?;
        ensure!(state.fence() == fence, "publication fenced");
        if matches!(memory.fault, Fault::PublicationBefore) {
            memory.fault = Fault::None;
            return Ok(CommitOutcome::Uncertain);
        }
        memory.revision += 1;
        memory.snapshot.publication = Some(Publication {
            resource_version: memory.revision.to_string(),
            fence: fence.into(),
            bytes: bytes.into(),
        });
        Ok(
            if matches!(std::mem::take(&mut memory.fault), Fault::PublicationAfter) {
                CommitOutcome::Uncertain
            } else {
                CommitOutcome::Committed
            },
        )
    }
}

fn node(pod: &str) -> Identity {
    Identity {
        kind: IdentityKind::Node,
        universe: "a".repeat(64),
        node: "b".repeat(64),
        pod_uid: pod.into(),
        boot_id: "c".repeat(64),
        pod_name: pod.into(),
        container_id: String::new(),
    }
}

fn cp(pod: &str) -> Identity {
    Identity {
        kind: IdentityKind::ControlPlane,
        universe: String::new(),
        node: String::new(),
        pod_uid: pod.into(),
        boot_id: "d".repeat(64),
        pod_name: pod.into(),
        container_id: String::new(),
    }
}

async fn manager(store: &Store, token: &str) -> CaManager<Store> {
    CaManager::acquire(
        store.clone(),
        Leadership::new(token.into()).unwrap(),
        SecurityOptions::new("system"),
        unix_now(),
    )
    .await
    .unwrap()
}

#[derive(Clone, Copy)]
enum IssuancePause {
    BeforeCommit,
    AfterCommit,
    Readback,
}

struct PausedStore {
    inner: Store,
    armed: Mutex<Option<IssuancePause>>,
    readback: std::sync::atomic::AtomicBool,
    captured: tokio::sync::Notify,
    resume: tokio::sync::Notify,
}

impl PausedStore {
    async fn pause(&self) {
        self.captured.notify_one();
        self.resume.notified().await;
    }
}

impl CaStore for Arc<PausedStore> {
    async fn read(&self) -> Result<StoreSnapshot> {
        let snapshot = self.inner.read().await?;
        if self
            .readback
            .swap(false, std::sync::atomic::Ordering::SeqCst)
        {
            self.pause().await;
        }
        Ok(snapshot)
    }
    async fn commit(&self, expected: &StoreSnapshot, next: &StateImage) -> Result<CommitOutcome> {
        let pause = self.armed.lock().unwrap().take();
        if matches!(pause, Some(IssuancePause::BeforeCommit)) {
            self.pause().await;
        }
        let result = self.inner.commit(expected, next).await?;
        if matches!(pause, Some(IssuancePause::AfterCommit)) {
            self.pause().await;
        }
        if matches!(pause, Some(IssuancePause::Readback)) {
            self.readback
                .store(true, std::sync::atomic::Ordering::SeqCst);
        }
        Ok(result)
    }
    async fn publish(
        &self,
        expected: &StoreSnapshot,
        fence: &str,
        bytes: &[u8],
    ) -> Result<CommitOutcome> {
        self.inner.publish(expected, fence, bytes).await
    }
}

#[tokio::test]
async fn delayed_issuance_cannot_extend_retired_root_authorization() {
    for pause in [
        IssuancePause::BeforeCommit,
        IssuancePause::AfterCommit,
        IssuancePause::Readback,
    ] {
        for unchanged_watermark in [false, true] {
            let store = Store::default();
            let gated = Arc::new(PausedStore {
                inner: store.clone(),
                armed: Mutex::new(None),
                readback: false.into(),
                captured: Default::default(),
                resume: Default::default(),
            });
            let now = unix_now();
            let options = SecurityOptions {
                leaf_lifetime: 100,
                clock_skew: 2,
                proof_lifetime: 1,
                ..SecurityOptions::new("system")
            };
            let first = Arc::new(
                CaManager::acquire(
                    gated.clone(),
                    Leadership::new("first".into()).unwrap(),
                    options.clone(),
                    now,
                )
                .await
                .unwrap(),
            );
            let key = generate_local_key().unwrap();
            let seed = first
                .issue(&key.csr_pem, node("seed"), false, now)
                .await
                .unwrap();
            let issue_at = now + if unchanged_watermark { 0 } else { 1 };
            *gated.armed.lock().unwrap() = Some(pause);
            let task = {
                let first = first.clone();
                tokio::spawn(async move {
                    first
                        .issue(&key.csr_pem, node("delayed"), false, issue_at)
                        .await
                })
            };
            tokio::time::timeout(std::time::Duration::from_secs(5), gated.captured.notified())
                .await
                .unwrap();
            // Do not cancel the predecessor's local term: durable fencing must
            // protect even a paused process that has not observed election loss.
            let second = CaManager::acquire(
                store.clone(),
                Leadership::new("second".into()).unwrap(),
                options,
                now + 2,
            )
            .await
            .unwrap();
            let bound = second
                .state()
                .await
                .unwrap()
                .expiry_watermarks()
                .next()
                .unwrap()
                .1;
            assert!(bound >= seed.not_after);
            if !matches!(pause, IssuancePause::BeforeCommit) {
                assert!(bound >= issue_at + 100);
            }
            second.begin_rotation(now).await.unwrap();
            let at = second.state().await.unwrap().published_at().unwrap();
            assert_eq!(
                second.advance_rotation(at + 3).await.unwrap(),
                Phase::Switched
            );
            assert_eq!(
                second.advance_rotation(bound + 1).await.unwrap(),
                Phase::Switched
            );
            assert_eq!(
                second.advance_rotation(bound + 2).await.unwrap(),
                Phase::Stable
            );
            gated.resume.notify_one();
            let result = task.await.unwrap();
            if matches!(pause, IssuancePause::Readback) {
                // A readback already captured before takeover may be delivered
                // late. Its leaf is still bounded and expired before retirement.
                assert!(result.unwrap().not_after <= bound);
            } else {
                assert!(result.is_err());
            }
            assert_ne!(
                second.state().await.unwrap().bundle().active,
                seed.root_digest
            );
        }
    }
}

#[tokio::test]
async fn durable_admission_prevents_rebinding_a_live_pod() {
    let store = Store::default();
    let manager = manager(&store, "term").await;
    let original = node("worker");
    manager.admit(original.clone()).await.unwrap();
    for changed_node in [false, true] {
        let mut next = original.clone();
        next.boot_id = "e".repeat(64);
        if changed_node {
            next.node = "f".repeat(64);
        } else {
            next.universe = "f".repeat(64);
        }
        // Live authorization, not issuance history, chooses a new identity.
        manager.admit(next).await.unwrap();
    }
    let mut same_identity = original.clone();
    same_identity.boot_id = "e".repeat(64);
    manager.admit(same_identity).await.unwrap();
    // A replacement Pod is a distinct participant and may join the new Site.
    let mut replacement = original;
    replacement.pod_uid = "replacement".into();
    replacement.universe = "f".repeat(64);
    manager.admit(replacement).await.unwrap();
    let image = manager.state().await.unwrap().to_image().unwrap();
    assert!(image.metadata.len() <= 16384);
    assert!(
        !String::from_utf8(image.metadata)
            .unwrap()
            .contains("worker")
    );
}

async fn issue(
    manager: &CaManager<Store>,
    identity: Identity,
    probe: bool,
) -> (IssuedCertificate, LocalKey) {
    let key = generate_local_key().unwrap();
    let issued = manager
        .issue(&key.csr_pem, identity, probe, unix_now())
        .await
        .unwrap();
    (issued, key)
}

fn snapshot(bundle: &TrustBundle, issued: &IssuedCertificate, key: &LocalKey) -> TlsSnapshot {
    TlsSnapshot::new(&bundle.json(), &issued.certificate_pem, &key.key_pem).unwrap()
}

fn client_config(
    bundle: &TrustBundle,
    leaf: Option<(&IssuedCertificate, &LocalKey)>,
) -> Arc<rustls::ClientConfig> {
    let mut roots = rustls::RootCertStore::empty();
    for cert in openssl::x509::X509::stack_from_pem(bundle.certificates.as_bytes()).unwrap() {
        roots
            .add(rustls::pki_types::CertificateDer::from(
                cert.to_der().unwrap(),
            ))
            .unwrap();
    }
    let builder = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_protocol_versions(&[&rustls::version::TLS13])
    .unwrap()
    .with_root_certificates(roots);
    let mut config = if let Some((issued, key)) = leaf {
        let cert = openssl::x509::X509::from_pem(&issued.certificate_pem).unwrap();
        let key = openssl::pkey::PKey::private_key_from_pem(&key.key_pem)
            .unwrap()
            .private_key_to_pkcs8()
            .unwrap();
        builder
            .with_client_auth_cert(
                vec![rustls::pki_types::CertificateDer::from(
                    cert.to_der().unwrap(),
                )],
                rustls::pki_types::PrivateKeyDer::Pkcs8(key.into()),
            )
            .unwrap()
    } else {
        builder.with_no_client_auth()
    };
    config.resumption = rustls::client::Resumption::disabled();
    Arc::new(config)
}

async fn node_proof(
    server: TlsSnapshot,
    bundle: &TrustBundle,
    issued: &IssuedCertificate,
    key: &LocalKey,
    boot: &str,
    issuer: &str,
    term: Leadership,
) -> Result<TlsProof> {
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let address = listener.local_addr()?;
    let task = tokio::spawn(async move {
        let (raw, _) = listener.accept().await?;
        let mut exchange = server.receive_node_proof(raw, &term).await?;
        exchange
            .stream
            .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            .await?;
        Ok::<_, anyhow::Error>(exchange.proof)
    });
    let raw = TcpStream::connect(address).await?;
    let mut stream = tokio_rustls::TlsConnector::from(client_config(bundle, Some((issued, key))))
        .connect(
            rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc")?,
            raw,
        )
        .await?;
    stream.write_all(format!("POST /v1/proof HTTP/1.1\r\nHost: racer-controlplane.system.svc\r\nX-Racer-Boot: {boot}\r\nX-Racer-Trust-Generation: {}\r\nX-Racer-Trust-Digest: {}\r\nX-Racer-Certificate-Issuer: {issuer}\r\nX-Racer-Old-Connections: 0\r\nContent-Length: 0\r\n\r\n", bundle.generation, bundle.digest()).as_bytes()).await?;
    task.await?
}

async fn replica_proof(
    observer: TlsSnapshot,
    server: TlsSnapshot,
    identity: &Identity,
    key: &LocalKey,
    term: Leadership,
) -> Result<TlsProof> {
    let ack = ReplicaAcknowledgment {
        pod_uid: identity.pod_uid.clone(),
        boot_id: identity.boot_id.clone(),
        csr_digest: digest(&key.csr_pem),
        generation: server.bundle().generation,
        digest: server.bundle().digest(),
        old_connections_drained: true,
    };
    let expected = ack.clone();
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let address = listener.local_addr()?;
    let task = tokio::spawn(async move {
        let (raw, _) = listener.accept().await?;
        let (mut stream, _) = server.accept(raw, false).await?;
        let mut request = Vec::new();
        while !request.ends_with(b"\r\n\r\n") {
            request.push(stream.read_u8().await?);
        }
        ensure!(
            request.starts_with(b"GET /v1/replica-proof HTTP/1.1"),
            "wrong proof route"
        );
        write_replica_ack(&mut stream, &ack).await
    });
    let proof = observer
        .probe_replica(
            TcpStream::connect(address).await?,
            "system",
            &expected,
            &term,
        )
        .await;
    task.await??;
    proof
}

#[tokio::test]
async fn issued_chain_passes_dataplane_openssl_strict_verification() {
    use openssl::x509::{X509, X509StoreContext, store::X509StoreBuilder, verify::X509VerifyFlags};
    let store = Store::default();
    let manager = manager(&store, "strict").await;
    let key = generate_local_key().unwrap();
    let issued = manager
        .issue(&key.csr_pem, node("strict"), false, unix_now())
        .await
        .unwrap();
    let mut roots = X509StoreBuilder::new().unwrap();
    roots
        .set_flags(X509VerifyFlags::X509_STRICT | X509VerifyFlags::CHECK_SS_SIGNATURE)
        .unwrap();
    for root in X509::stack_from_pem(issued.bundle.certificates.as_bytes()).unwrap() {
        roots.add_cert(root).unwrap();
    }
    let roots = roots.build();
    let leaf = X509::from_pem(&issued.certificate_pem).unwrap();
    let chain = openssl::stack::Stack::new().unwrap();
    let mut context = X509StoreContext::new().unwrap();
    assert!(
        context
            .init(&roots, &leaf, &chain, |ctx| ctx.verify_cert())
            .unwrap(),
        "{}",
        context.error()
    );
}

#[tokio::test]
async fn issuance_uncertain_commit_restart_and_fencing() {
    let store = Store::default();
    let first = manager(&store, "first").await;
    let key = generate_local_key().unwrap();
    store.fault(Fault::Before);
    assert!(
        first
            .issue(&key.csr_pem, node("before"), false, unix_now())
            .await
            .is_err()
    );
    assert_eq!(
        first
            .state()
            .await
            .unwrap()
            .expiry_watermarks()
            .next()
            .unwrap()
            .1,
        0
    );

    store.fault(Fault::After);
    let issued = first
        .issue(&key.csr_pem, node("after"), false, unix_now())
        .await
        .unwrap();
    assert_eq!(
        issued.not_after,
        first
            .state()
            .await
            .unwrap()
            .expiry_watermarks()
            .next()
            .unwrap()
            .1
    );
    store.fault(Fault::Readback);
    assert!(
        first
            .issue(&key.csr_pem, node("unknown"), false, unix_now())
            .await
            .is_err()
    );
    // Write may have committed despite no certificate being returned. Restart
    // retains the watermark rather than regenerating a CA.
    let second = manager(&store, "second").await;
    let state = second.state().await.unwrap();
    assert!(
        state
            .expiry_watermarks()
            .any(|(_, expiry)| expiry >= issued.not_after)
    );
    assert_eq!(state.bundle().active, issued.root_digest);
    assert!(
        first
            .issue(&key.csr_pem, node("stale"), false, unix_now())
            .await
            .is_err()
    );
    second.leadership().cancel();
    assert!(
        second
            .issue(&key.csr_pem, node("canceled"), false, unix_now())
            .await
            .is_err()
    );
}

#[tokio::test]
async fn retirement_candidates_bind_the_observation_term_and_original_identity() {
    let store = Store::default();
    let first = manager(&store, "first").await;
    let identity = node("existing");
    let (issued, _) = issue(&first, identity.clone(), false).await;
    first.begin_rotation(unix_now()).await.unwrap();
    let published = first.state().await.unwrap().published_at().unwrap();
    let second = manager(&store, "second").await;
    assert!(
        first
            .advance_rotation(issued.not_after + 300)
            .await
            .is_err()
    );
    let state = second.state().await.unwrap();
    assert_eq!(state.published_at(), Some(published));
    assert!(
        state
            .expiry_watermarks()
            .any(|(root, expiry)| root == issued.root_digest && expiry == issued.not_after)
    );
    assert_eq!(
        second.advance_rotation(published + 599).await.unwrap(),
        Phase::Overlap
    );
    assert_eq!(
        second.advance_rotation(published + 600).await.unwrap(),
        Phase::Switched
    );
    assert_eq!(
        second
            .advance_rotation(issued.not_after + 299)
            .await
            .unwrap(),
        Phase::Switched
    );
    assert_eq!(
        second
            .advance_rotation(issued.not_after + 300)
            .await
            .unwrap(),
        Phase::Stable
    );
}

#[tokio::test]
async fn lost_private_state_and_unsupported_metadata_never_bootstrap() {
    let store = Store::default();
    let first = manager(&store, "first").await;
    issue(&first, node("member"), false).await;
    let mut image = store.read().await.unwrap().image.unwrap();
    let mut metadata: serde_json::Value = serde_json::from_slice(&image.metadata).unwrap();
    metadata["version"] = serde_json::json!(5);
    image.metadata = serde_json::to_vec(&metadata).unwrap();
    assert!(CaState::from_image(&image).is_err());
    {
        let mut memory = store.0.lock().unwrap();
        memory.snapshot.image = None;
        memory.snapshot.resource_version = None;
        memory.snapshot.publication.as_mut().unwrap().bytes.clear();
    }
    assert!(
        CaManager::acquire(
            store.clone(),
            Leadership::new("replacement".into()).unwrap(),
            SecurityOptions::new("system"),
            unix_now()
        )
        .await
        .is_err()
    );
    {
        let mut memory = store.0.lock().unwrap();
        memory.snapshot.publication = None;
        memory.snapshot.prior_artifacts = true;
    }
    assert!(
        CaManager::acquire(
            store,
            Leadership::new("replacement".into()).unwrap(),
            SecurityOptions::new("system"),
            unix_now()
        )
        .await
        .is_err()
    );
}

#[tokio::test]
async fn real_tls_rotation_requires_every_boot_fresh_term_and_expiry() {
    let store = Store::default();
    let first = manager(&store, "first").await;
    let node_id = node("worker");
    let cp_id = cp("replica");
    let (old_node, node_key) = issue(&first, node_id.clone(), false).await;
    let old_root = old_node.root_digest.clone();
    first.begin_rotation(unix_now()).await.unwrap();
    let (probe_cp, cp_key) = issue(&first, cp_id.clone(), true).await;
    let bundle = first.state().await.unwrap().bundle();
    assert_eq!(bundle.active, old_root);
    assert_ne!(probe_cp.root_digest, old_root);

    let proof = node_proof(
        snapshot(&bundle, &probe_cp, &cp_key),
        &bundle,
        &old_node,
        &node_key,
        &node_id.boot_id,
        &old_root,
        first.leadership(),
    )
    .await
    .unwrap();
    first
        .record_proof(&node_id.key(), proof, unix_now())
        .await
        .unwrap();
    assert_eq!(
        first.advance_rotation(unix_now()).await.unwrap(),
        Phase::Overlap,
        "publication overlap delay must elapse"
    );
    let proof = replica_proof(
        snapshot(&bundle, &probe_cp, &cp_key),
        snapshot(&bundle, &probe_cp, &cp_key),
        &cp_id,
        &cp_key,
        first.leadership(),
    )
    .await
    .unwrap();
    // Same-second takeover must reject capabilities minted under the old term.
    let second = manager(&store, "second").await;
    assert!(
        second
            .record_proof(&cp_id.key(), proof, unix_now())
            .await
            .is_err()
    );
    assert_eq!(
        second.advance_rotation(unix_now()).await.unwrap(),
        Phase::Overlap
    );

    let proof = node_proof(
        snapshot(&bundle, &probe_cp, &cp_key),
        &bundle,
        &old_node,
        &node_key,
        &node_id.boot_id,
        &old_root,
        second.leadership(),
    )
    .await
    .unwrap();
    second
        .record_proof(&node_id.key(), proof, unix_now())
        .await
        .unwrap();
    let proof = replica_proof(
        snapshot(&bundle, &probe_cp, &cp_key),
        snapshot(&bundle, &probe_cp, &cp_key),
        &cp_id,
        &cp_key,
        second.leadership(),
    )
    .await
    .unwrap();
    second
        .record_proof(&cp_id.key(), proof, unix_now())
        .await
        .unwrap();
    assert_eq!(
        second
            .advance_rotation(second.state().await.unwrap().published_at().unwrap() + 600)
            .await
            .unwrap(),
        Phase::Switched
    );
    let bundle = second.state().await.unwrap().bundle();
    assert_ne!(bundle.active, old_root);
    // Old-root node TLS remains locally authenticated until expiry. Proofs are
    // diagnostics and cannot block or accelerate rotation.
    let proof = node_proof(
        snapshot(&bundle, &probe_cp, &cp_key),
        &bundle,
        &old_node,
        &node_key,
        &node_id.boot_id,
        &old_root,
        second.leadership(),
    )
    .await
    .unwrap();
    second
        .record_proof(&node_id.key(), proof, unix_now())
        .await
        .unwrap();

    let (new_node, new_key) = issue(&second, node_id.clone(), false).await;
    let proof = node_proof(
        snapshot(&bundle, &probe_cp, &cp_key),
        &bundle,
        &new_node,
        &new_key,
        &node_id.boot_id,
        &new_node.root_digest,
        second.leadership(),
    )
    .await
    .unwrap();
    second
        .record_proof(&node_id.key(), proof, unix_now())
        .await
        .unwrap();
    let proof = replica_proof(
        snapshot(&bundle, &probe_cp, &cp_key),
        snapshot(&bundle, &probe_cp, &cp_key),
        &cp_id,
        &cp_key,
        second.leadership(),
    )
    .await
    .unwrap();
    second
        .record_proof(&cp_id.key(), proof, unix_now())
        .await
        .unwrap();
    assert_eq!(
        second.advance_rotation(unix_now()).await.unwrap(),
        Phase::Switched,
        "expiry watermark blocks retirement"
    );
    let later = old_node.not_after + 301;
    assert_eq!(
        second.advance_rotation(later).await.unwrap(),
        Phase::Stable,
        "offline participants never block expiry-based retirement"
    );
    assert_eq!(second.advance_rotation(later).await.unwrap(), Phase::Stable);
    assert_eq!(second.state().await.unwrap().expiry_watermarks().count(), 1);
    assert!(
        second
            .state()
            .await
            .unwrap()
            .to_image()
            .unwrap()
            .metadata
            .len()
            <= 16384
    );
}

#[tokio::test]
async fn tls_proof_cannot_cross_boot_or_forge_issuer() {
    let store = Store::default();
    let manager = manager(&store, "term").await;
    let id = node("node");
    let (leaf, key) = issue(&manager, id.clone(), false).await;
    let (cp_leaf, cp_key) = issue(&manager, cp("cp"), false).await;
    let bundle = manager.state().await.unwrap().bundle();
    let mut other = id.clone();
    other.boot_id = "e".repeat(64);
    issue(&manager, other.clone(), false).await;
    let proof = node_proof(
        snapshot(&bundle, &cp_leaf, &cp_key),
        &bundle,
        &leaf,
        &key,
        &other.boot_id,
        &leaf.root_digest,
        manager.leadership(),
    )
    .await;
    assert!(
        proof.is_err(),
        "boot header must match signed claims at transport boundary"
    );
    assert!(
        node_proof(
            snapshot(&bundle, &cp_leaf, &cp_key),
            &bundle,
            &leaf,
            &key,
            &id.boot_id,
            &"f".repeat(64),
            manager.leadership()
        )
        .await
        .is_err()
    );

    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let server = snapshot(&bundle, &cp_leaf, &cp_key);
    let task = tokio::spawn(async move {
        server
            .accept(listener.accept().await.unwrap().0, true)
            .await
    });
    let raw = TcpStream::connect(address).await.unwrap();
    let client = tokio_rustls::TlsConnector::from(client_config(&bundle, None))
        .connect(
            rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
            raw,
        )
        .await;
    drop(client);
    assert!(
        task.await.unwrap().is_err(),
        "anonymous TLS cannot authenticate control"
    );

    // A server-auth-only CP leaf must not authenticate as a node client.
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let server = snapshot(&bundle, &cp_leaf, &cp_key);
    let task = tokio::spawn(async move {
        server
            .accept(listener.accept().await.unwrap().0, true)
            .await
    });
    let raw = TcpStream::connect(address).await.unwrap();
    let client =
        tokio_rustls::TlsConnector::from(client_config(&bundle, Some((&cp_leaf, &cp_key))))
            .connect(
                rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
                raw,
            )
            .await;
    drop(client);
    assert!(
        task.await.unwrap().is_err(),
        "CP serverAuth leaf accepted as client"
    );

    let foreign_store = Store::default();
    let foreign = CaManager::acquire(
        foreign_store,
        Leadership::new("foreign".into()).unwrap(),
        SecurityOptions::new("system"),
        unix_now(),
    )
    .await
    .unwrap();
    let foreign_bundle = foreign.state().await.unwrap().bundle();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let server = snapshot(&bundle, &cp_leaf, &cp_key);
    let task = tokio::spawn(async move {
        server
            .accept(listener.accept().await.unwrap().0, false)
            .await
    });
    let raw = TcpStream::connect(address).await.unwrap();
    let client = tokio_rustls::TlsConnector::from(client_config(&foreign_bundle, None))
        .connect(
            rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
            raw,
        )
        .await;
    assert!(client.is_err(), "server outside trust domain accepted");
    assert!(task.await.unwrap().is_err());
}

#[tokio::test]
async fn trust_publication_and_hot_tls_preserve_last_good_and_drain() {
    let store = Store::default();
    let manager = manager(&store, "term").await;
    store.fault(Fault::PublicationAfter);
    manager.publish().await.unwrap();
    let (leaf, key) = issue(&manager, cp("replica"), false).await;
    let first = leaf.bundle.clone();
    assert_eq!(TrustBundle::parse(&first.json()).unwrap(), first);
    let mut noncanonical = first.json();
    noncanonical.push(b'\n');
    assert!(TrustBundle::parse(&noncanonical).is_err());
    let hot = HotTls::default();
    hot.install(snapshot(&first, &leaf, &key), snapshot(&first, &leaf, &key))
        .unwrap();
    assert!(hot.ready(unix_now(), true, true));
    assert!(!hot.ready(unix_now(), true, false));
    let (accepted, guard) = hot.production_connection().unwrap();
    assert!(hot.is_current_epoch(&accepted));
    manager.begin_rotation(unix_now()).await.unwrap();
    let (probe, probe_key) = issue(&manager, cp("replica"), true).await;
    let overlap = probe.bundle.clone();
    hot.install(
        snapshot(&overlap, &leaf, &key),
        snapshot(&overlap, &probe, &probe_key),
    )
    .unwrap();
    assert!(!hot.drained());
    assert!(
        !hot.is_current_epoch(&accepted),
        "an old snapshot accepted before install cannot capture the new drain epoch"
    );
    assert!(hot.is_current_epoch(&hot.snapshot().unwrap()));
    assert!(
        hot.install(snapshot(&first, &leaf, &key), snapshot(&first, &leaf, &key))
            .is_err()
    );
    assert_eq!(hot.snapshot().unwrap().production.bundle(), &overlap);
    assert!(TlsSnapshot::new(&overlap.json(), &leaf.certificate_pem, &probe_key.key_pem).is_err());
    drop(guard);
    assert!(hot.drained());
}

#[tokio::test]
async fn old_root_retires_with_live_processes_only_after_fresh_switched_tls() {
    let store = Store::default();
    let mut options = SecurityOptions::new("system");
    options.leaf_lifetime = 3;
    options.clock_skew = 0;
    options.proof_lifetime = 1;
    let manager = CaManager::acquire(
        store.clone(),
        Leadership::new("term".into()).unwrap(),
        options,
        unix_now(),
    )
    .await
    .unwrap();
    let node_id = node("worker");
    let cp_id = cp("standby");
    let (old, key) = issue(&manager, node_id.clone(), false).await;
    manager.begin_rotation(unix_now()).await.unwrap();
    let (probe, probe_key) = issue(&manager, cp_id.clone(), true).await;
    let bundle = probe.bundle.clone();
    let proof = node_proof(
        snapshot(&bundle, &probe, &probe_key),
        &bundle,
        &old,
        &key,
        &node_id.boot_id,
        &old.root_digest,
        manager.leadership(),
    )
    .await
    .unwrap();
    manager
        .record_proof(&node_id.key(), proof, unix_now())
        .await
        .unwrap();
    let proof = replica_proof(
        snapshot(&bundle, &probe, &probe_key),
        snapshot(&bundle, &probe, &probe_key),
        &cp_id,
        &probe_key,
        manager.leadership(),
    )
    .await
    .unwrap();
    manager
        .record_proof(&cp_id.key(), proof, unix_now())
        .await
        .unwrap();
    assert_eq!(
        manager
            .advance_rotation(manager.state().await.unwrap().published_at().unwrap() + 1)
            .await
            .unwrap(),
        Phase::Switched
    );
    while unix_now() <= old.not_after {
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    }
    // No proof or online fleet census is required after the expiry floor.
    assert_eq!(
        manager.advance_rotation(unix_now()).await.unwrap(),
        Phase::Stable
    );
    let (new, key) = issue(&manager, node_id.clone(), false).await;
    let (probe, probe_key) = issue(&manager, cp_id.clone(), true).await;
    let bundle = probe.bundle.clone();
    let proof = node_proof(
        snapshot(&bundle, &probe, &probe_key),
        &bundle,
        &new,
        &key,
        &node_id.boot_id,
        &new.root_digest,
        manager.leadership(),
    )
    .await
    .unwrap();
    manager
        .record_proof(&node_id.key(), proof, unix_now())
        .await
        .unwrap();
    let proof = replica_proof(
        snapshot(&bundle, &probe, &probe_key),
        snapshot(&bundle, &probe, &probe_key),
        &cp_id,
        &probe_key,
        manager.leadership(),
    )
    .await
    .unwrap();
    manager
        .record_proof(&cp_id.key(), proof, unix_now())
        .await
        .unwrap();
    assert_eq!(
        manager.advance_rotation(unix_now()).await.unwrap(),
        Phase::Stable
    );
    assert!(
        manager
            .state()
            .await
            .unwrap()
            .to_image()
            .unwrap()
            .metadata
            .len()
            <= 16384
    );
    assert_ne!(
        manager.state().await.unwrap().bundle().active,
        old.root_digest
    );
}

#[tokio::test]
async fn persisted_overlap_repairs_publication_after_restart() {
    let store = Store::default();
    let first = manager(&store, "first").await;
    let original_publication = store.read().await.unwrap().publication;
    first.admit(node("idle")).await.unwrap();
    first.begin_rotation(unix_now()).await.unwrap();
    let overlap = first.state().await.unwrap().bundle();
    // Model a crash after the Secret transition and before its ConfigMap write.
    store.0.lock().unwrap().snapshot.publication = original_publication;
    let key = generate_local_key().unwrap();
    assert!(
        first
            .issue(&key.csr_pem, node("idle"), false, unix_now())
            .await
            .is_err()
    );
    let second = manager(&store, "second").await;
    assert_eq!(second.state().await.unwrap().bundle(), overlap);
    assert_eq!(
        store.read().await.unwrap().publication.unwrap().bytes,
        overlap.json()
    );
    assert_eq!(
        second.advance_rotation(unix_now()).await.unwrap(),
        Phase::Overlap
    );
}

#[tokio::test]
async fn overlap_delay_starts_only_after_confirmed_publication_and_survives_takeover() {
    let store = Store::default();
    let first = manager(&store, "first").await;
    first.begin_rotation(unix_now()).await.unwrap();
    // Model a crash between the overlap Secret CAS and public CAS. Neither a
    // failed publication nor time alone can authorize the issuer switch.
    {
        let mut memory = store.0.lock().unwrap();
        let image = memory.snapshot.image.as_mut().unwrap();
        let mut metadata: serde_json::Value = serde_json::from_slice(&image.metadata).unwrap();
        metadata["published_at"] = serde_json::Value::Null;
        image.metadata = serde_json::to_vec(&metadata).unwrap();
        memory.snapshot.publication = None;
    }
    store.fault(Fault::PublicationBefore);
    assert!(first.publish().await.is_err());
    assert_eq!(first.state().await.unwrap().published_at(), None);
    assert!(
        first
            .advance_rotation(unix_now() + 1_000_000)
            .await
            .is_err()
    );
    let second = manager(&store, "second").await;
    let at = second.state().await.unwrap().published_at().unwrap();
    assert_eq!(
        second.advance_rotation(at + 599).await.unwrap(),
        Phase::Overlap
    );
    let third = manager(&store, "third").await;
    assert_eq!(third.state().await.unwrap().published_at(), Some(at));
    assert_eq!(
        third.advance_rotation(at + 600).await.unwrap(),
        Phase::Switched
    );
}

#[tokio::test]
async fn signed_process_claims_are_exact_and_old_leaves_need_no_issuance_ledger() {
    let store = Store::default();
    let manager = manager(&store, "term").await;
    let identity = node("worker");
    let (leaf, key) = issue(&manager, identity.clone(), false).await;
    let claims = certificate_claims(&leaf.certificate_pem).unwrap();
    assert_eq!(claims.namespace, "system");
    assert_eq!(claims.identity, identity);
    assert!(SignedClaims::parse("spiffe://racer/controlplane").is_err());
    let mut wrong = claims.clone();
    wrong.identity.boot_id.clear();
    assert!(wrong.uri().is_err());
    let (server, server_key) = issue(&manager, cp("cp"), false).await;
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let tls = snapshot(&leaf.bundle, &server, &server_key);
    let task = tokio::spawn(async move {
        tls.accept(listener.accept().await.unwrap().0, true)
            .await
            .unwrap()
            .1
            .unwrap()
    });
    let stream = tokio_rustls::TlsConnector::from(client_config(&leaf.bundle, Some((&leaf, &key))))
        .connect(
            rustls::pki_types::ServerName::try_from("racer-controlplane.system.svc").unwrap(),
            TcpStream::connect(address).await.unwrap(),
        )
        .await
        .unwrap();
    let peer = task.await.unwrap();
    let state = manager.state().await.unwrap();
    assert_eq!(state.verify_peer(&peer, unix_now()).unwrap(), &identity);
    assert!(state.verify_peer(&peer, leaf.not_after).is_err());
    assert!(state.to_image().unwrap().metadata.len() <= 16384);
    drop(stream);
}

#[tokio::test]
async fn csr_algorithms_signature_validation_and_hostile_extensions() {
    use openssl::{
        ec::{EcGroup, EcKey},
        hash::MessageDigest,
        nid::Nid,
        pkey::{Id, PKey},
        rsa::Rsa,
        stack::Stack,
        x509::{
            X509, X509NameBuilder, X509Req,
            extension::{BasicConstraints, SubjectAlternativeName},
        },
    };
    let store = Store::default();
    let manager = manager(&store, "term").await;
    let mut keys = vec![
        PKey::from_rsa(Rsa::generate(2048).unwrap()).unwrap(),
        PKey::generate_ed25519().unwrap(),
    ];
    for curve in [Nid::X9_62_PRIME256V1, Nid::SECP384R1, Nid::SECP521R1] {
        keys.push(
            PKey::from_ec_key(EcKey::generate(&EcGroup::from_curve_name(curve).unwrap()).unwrap())
                .unwrap(),
        );
    }
    for key in keys {
        let mut csr = X509Req::builder().unwrap();
        let mut name = X509NameBuilder::new().unwrap();
        name.append_entry_by_text("CN", "system:masters").unwrap();
        csr.set_subject_name(&name.build()).unwrap();
        csr.set_pubkey(&key).unwrap();
        let mut extensions = Stack::new().unwrap();
        extensions
            .push(BasicConstraints::new().critical().ca().build().unwrap())
            .unwrap();
        extensions
            .push(
                SubjectAlternativeName::new()
                    .dns("evil.example")
                    .uri("spiffe://evil/admin")
                    .build(&csr.x509v3_context(None))
                    .unwrap(),
            )
            .unwrap();
        csr.add_extensions(&extensions).unwrap();
        csr.sign(
            &key,
            if key.id() == Id::ED25519 {
                MessageDigest::null()
            } else {
                MessageDigest::sha256()
            },
        )
        .unwrap();
        let csr = csr.build();
        for identity in [node("worker"), cp("replica")] {
            let issued = manager
                .issue(&csr.to_pem().unwrap(), identity.clone(), false, unix_now())
                .await
                .unwrap();
            let cert = X509::from_pem(&issued.certificate_pem).unwrap();
            assert_eq!(cert.subject_name().entries().count(), 0);
            let der = cert.to_der().unwrap();
            let (_, parsed) = x509_parser::parse_x509_certificate(&der).unwrap();
            assert!(!parsed.is_ca());
            let eku = parsed.extended_key_usage().unwrap().unwrap();
            assert!(eku.value.server_auth);
            assert_eq!(eku.value.client_auth, identity.kind == IdentityKind::Node);
            let sans = cert.subject_alt_names().unwrap();
            assert_eq!(
                sans.iter().filter_map(|s| s.uri()).collect::<Vec<_>>(),
                vec![
                    SignedClaims {
                        version: 1,
                        namespace: "system".into(),
                        identity: identity.clone()
                    }
                    .uri()
                    .unwrap()
                ]
            );
            let dns: Vec<_> = sans.iter().filter_map(|s| s.dnsname()).collect();
            assert_eq!(
                dns,
                if identity.kind == IdentityKind::Node {
                    vec![]
                } else {
                    vec!["racer-controlplane.system.svc"]
                }
            );
            assert!(
                certificate_matches_csr(&issued.certificate_pem, &csr.to_pem().unwrap()).unwrap()
            );
        }
        let mut corrupted = csr.to_der().unwrap();
        *corrupted.last_mut().unwrap() ^= 1;
        assert!(validate_csr(&corrupted).is_err());
    }
    let weak = PKey::from_rsa(Rsa::generate(1024).unwrap()).unwrap();
    let mut csr = X509Req::builder().unwrap();
    csr.set_subject_name(&X509NameBuilder::new().unwrap().build())
        .unwrap();
    csr.set_pubkey(&weak).unwrap();
    csr.sign(&weak, MessageDigest::sha256()).unwrap();
    assert!(validate_csr(&csr.build().to_pem().unwrap()).is_err());
}

#[tokio::test]
async fn ten_thousand_idle_participants_round_trip_sharded_state() {
    // The old sharded-state scale scenario now enforces zero fleet persistence.
    let store = Store::default();
    let manager = manager(&store, "term").await;
    let image = store.read().await.unwrap().image.unwrap();
    for n in 0..10_000 {
        let identity = node(&format!("pod-{n}"));
        manager.admit(identity).await.unwrap();
    }
    assert!(store.read().await.unwrap().image.unwrap() == image);
    // Actual issuance also changes only a fixed-size expiry watermark, never a
    // fingerprint ledger or Pod/boot history.
    let key = generate_local_key().unwrap();
    let now = unix_now();
    for n in 0..100 {
        manager
            .issue(&key.csr_pem, node(&format!("issued-{n}")), false, now)
            .await
            .unwrap();
    }
    let state = manager.state().await.unwrap();
    let encoded = state.to_image().unwrap();
    assert!(encoded.metadata.len() < 16384);
    assert!(encoded.metadata.len() <= image.metadata.len() + 10);
    assert!(CaState::from_image(&encoded).unwrap().to_image().unwrap() == encoded);
}

#[test]
fn enrollment_requires_bound_audience_and_actual_ownership() {
    let prefix = "racer.unbounded-cloud.io/";
    let review = TokenReviewResult {
        authenticated: true,
        audiences: vec![CONTROL_AUDIENCE.into()],
        pod_uids: vec!["pod".into()],
        error: String::new(),
    };
    let site = SiteData {
        metadata: ObjectMetadata {
            name: "edge".into(),
            uid: "site".into(),
            ..Default::default()
        },
    };
    let node = ObjectMetadata {
        name: "worker".into(),
        uid: "node".into(),
        labels: [
            ("unbounded-cloud.io/site".into(), "edge".into()),
            ("kubernetes.io/os".into(), "linux".into()),
        ]
        .into(),
        ..Default::default()
    };
    let daemon = WorkloadData {
        metadata: ObjectMetadata {
            namespace: "system".into(),
            name: "racer-dataplane".into(),
            uid: "ds".into(),
            labels: [(format!("{prefix}component"), "racer-dataplane".into())].into(),
            ..Default::default()
        },
        template_service_account: "racer-dataplane".into(),
        template_labels: [
            (format!("{prefix}component"), "racer-dataplane".into()),
            (format!("{prefix}dataplane"), "true".into()),
        ]
        .into(),
    };
    let pod = PodData {
        metadata: ObjectMetadata {
            namespace: "system".into(),
            name: "dataplane".into(),
            uid: "pod".into(),
            labels: [
                (format!("{prefix}dataplane"), "true".into()),
                (format!("{prefix}component"), "racer-dataplane".into()),
            ]
            .into(),
            owners: vec![OwnerReference {
                api_version: "apps/v1".into(),
                kind: "DaemonSet".into(),
                name: "racer-dataplane".into(),
                uid: "ds".into(),
                controller: true,
            }],
            ..Default::default()
        },
        service_account: "racer-dataplane".into(),
        node_name: "worker".into(),
        running_container_id: String::new(),
    };
    let boot = "a".repeat(64);
    let mut data = EnrollmentData {
        namespace: "system",
        pod_name: "dataplane",
        boot: &boot,
        review: &review,
        pod: &pod,
        daemon_set: &daemon,
        node: &node,
        site: &site,
    };
    let id = authorize_enrollment(&data).unwrap();
    assert_eq!(id.node, racer_identity("node", "node"));
    assert_eq!(id.universe, racer_identity("universe", "edge"));
    let mut forged = daemon.clone();
    forged.metadata.uid = "forged".into();
    data.daemon_set = &forged;
    assert!(authorize_enrollment(&data).is_err());
    data.daemon_set = &daemon;
    let mut wrong = review.clone();
    wrong.audiences = vec!["racer-peer".into()];
    data.review = &wrong;
    assert!(authorize_enrollment(&data).is_err());
    data.review = &review;
    let mut disabled = site.clone();
    disabled.metadata.deleting = true;
    data.site = &disabled;
    assert!(authorize_enrollment(&data).is_err());
    let mut canonical = node.clone();
    canonical
        .labels
        .insert("unbounded-cloud.io/site".into(), String::new());
    canonical
        .labels
        .insert("net.unbounded-cloud.io/site".into(), "edge".into());
    assert!(node_site(&canonical).is_empty());
    canonical.labels.remove("unbounded-cloud.io/site");
    assert!(node_site(&canonical).is_empty());
    assert_eq!(
        token_review_request("token").unwrap()["spec"]["audiences"],
        serde_json::json!(["racer-control"])
    );
}

#[tokio::test]
async fn standby_authorization_and_pending_process_retirement_require_live_proof() {
    let component = "racer-controlplane";
    let labels = [(
        "racer.unbounded-cloud.io/component".into(),
        component.into(),
    )]
    .into();
    let deployment = WorkloadData {
        metadata: ObjectMetadata {
            namespace: "system".into(),
            name: component.into(),
            uid: "deployment".into(),
            labels,
            ..Default::default()
        },
        template_service_account: component.into(),
        ..Default::default()
    };
    let replica_set = WorkloadData {
        metadata: ObjectMetadata {
            namespace: "system".into(),
            name: "replicas".into(),
            uid: "rs".into(),
            owners: vec![OwnerReference {
                api_version: "apps/v1".into(),
                kind: "Deployment".into(),
                name: component.into(),
                uid: "deployment".into(),
                controller: true,
            }],
            ..Default::default()
        },
        ..Default::default()
    };
    let mut pod = PodData {
        metadata: ObjectMetadata {
            namespace: "system".into(),
            name: "standby".into(),
            uid: "pod".into(),
            labels: deployment.metadata.labels.clone(),
            owners: vec![OwnerReference {
                api_version: "apps/v1".into(),
                kind: "ReplicaSet".into(),
                name: "replicas".into(),
                uid: "rs".into(),
                controller: true,
            }],
            ..Default::default()
        },
        service_account: component.into(),
        ..Default::default()
    };
    let identity =
        authorize_replica("system", &pod, &replica_set, &deployment, &"a".repeat(64)).unwrap();
    pod.metadata.owners[0].uid = "forged".into();
    assert!(authorize_replica("system", &pod, &replica_set, &deployment, &"a".repeat(64)).is_err());
    pod.metadata.owners[0].uid = "rs".into();
    let store = Store::default();
    let manager = manager(&store, "term").await;
    let mut pending = identity.clone();
    pending.boot_id = "pending".into();
    assert!(manager.admit(pending.clone()).await.is_err());
    let (leaf, key) = issue(&manager, identity.clone(), false).await;
    let before = manager.state().await.unwrap().to_image().unwrap();
    let bundle = leaf.bundle.clone();
    let proof = replica_proof(
        snapshot(&bundle, &leaf, &key),
        snapshot(&bundle, &leaf, &key),
        &identity,
        &key,
        manager.leadership(),
    )
    .await
    .unwrap();
    manager
        .record_proof(&identity.key(), proof, unix_now())
        .await
        .unwrap();
    assert!(manager.admit(pending).await.is_err());
    assert!(manager.state().await.unwrap().to_image().unwrap() == before);
    pod.metadata.deleting = true;
    assert!(
        authorize_replica("system", &pod, &replica_set, &deployment, &identity.boot_id).is_err()
    );
}
