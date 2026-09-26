//! Real TLS enrollment/publication fixture driving the production application graph.
use super::*;
use crate::control::wire;
use std::{
    io::{Read, Write},
    os::unix::fs::symlink,
    path::PathBuf,
    thread,
};

pub(super) fn local_worker(
    config: &Config,
    node: &Arc<NodeState>,
    id: u16,
) -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    let worker = WorkerId(id);
    let admission = Rc::new(Admission::new(config.limits.clone()));
    let (io, engine) = crate::runtime::crypto::pair(worker, 0, config.limits.queue_entries);
    let runtime = WorkerRuntime {
        reactor: Rc::new(Reactor::new(admission.clone())),
        admission,
        crypto: Rc::new(crate::runtime::crypto::CryptoClient::new(io)),
    };
    let local = WorkerRuntime {
        reactor: runtime.reactor.clone(),
        admission: runtime.admission.clone(),
        crypto: runtime.crypto.clone(),
    };
    let mut app = WorkerApplication::assemble(config, node, worker, local).unwrap();
    app.node = Some(node.clone());
    (
        app,
        runtime,
        PageCryptoEngine::new(CryptoRuntime { port: engine }),
    )
}

pub(super) fn definition() -> crate::control::caches::CacheDefinition {
    let (client_socket, origin_socket) =
        crate::control::caches::canonical_socket_paths("app-lifecycle").unwrap();
    crate::control::caches::CacheDefinition {
        id: crate::model::identity::CacheId("33333333-3333-4333-8333-333333333333".into()),
        name: "app-lifecycle".into(),
        client_socket,
        origin_socket,
        socket_mode: 0o600,
    }
}

pub(super) fn publication(
    config: &Config,
    sequence: u64,
    caches: Vec<crate::control::caches::CacheDefinition>,
) -> wire::Publication {
    wire::Publication {
        schema_version: 1,
        cluster: config.cluster.clone(),
        sequence: wire::PublicationSequence(sequence),
        membership_version: crate::model::identity::MembershipVersion(1),
        members: vec![crate::topology::membership::Member {
            node: config.node.clone(),
            shares: std::num::NonZeroU32::new(1).unwrap(),
            peer_endpoint: "127.0.0.1:7443".into(),
            rails: vec![],
            alignment_enabled: false,
        }],
        caches,
    }
}

fn page(app: &WorkerApplication) -> crate::memory::page::PageResult {
    use crate::memory::pool::{VerifiedBytes, VerifiedPage};
    use crate::model::{
        envelope::*, identity::*, limits::ResourceClass, metadata::VersionMetadata,
    };
    let version = ObjectVersion {
        object: ObjectId {
            cache: definition().id,
            key: CacheKey([0; 32]),
        },
        etag: StrongEtag::test_value("one"),
    };
    let id = PageId {
        version: version.clone(),
        number: PageNumber(0),
    };
    let cache = &version.object.cache;
    let plaintext = VerifiedPage {
        inner: Arc::new(VerifiedBytes {
            page: id.clone(),
            bytes: vec![1; 3],
            reservation: app
                .runtime
                .admission
                .reserve(Some(cache), ResourceClass::Plaintext, 3)
                .unwrap(),
        }),
    };
    let ciphertext = BufferPool::new(app.runtime.admission.clone())
        .ciphertext(
            app.runtime
                .admission
                .reserve(Some(cache), ResourceClass::Ciphertext, 19)
                .unwrap(),
            PageEnvelope {
                page: id,
                key_id: KeyId([7; 16]),
                nonce: Nonce([2; 24]),
                plaintext_length: 3,
                ciphertext_length: 19,
            },
            vec![2; 19],
        )
        .unwrap();
    crate::memory::page::PageResult {
        plaintext,
        ciphertext,
        metadata: VersionMetadata { version, length: 3 }.for_pin(),
    }
}

#[test]
fn two_worker_removal_waits_for_late_driver_and_blocks_late_memory_and_disk_fill() {
    use crate::control::caches::CacheLifecycle;
    let mut fixture = Fixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0), WorkerId(1)], 64).unwrap());
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap();
    let (mut first, rt0, mut crypto0) = local_worker(&config, &node, 0);
    let (mut second, rt1, mut crypto1) = local_worker(&config, &node, 1);
    first
        .telemetry
        .attach_io(rt0.reactor.clone(), rt0.admission.clone())
        .unwrap();
    for app in [&mut first, &mut second] {
        futures::executor::block_on(app.store.writer.open()).unwrap();
        app.caches = vec![definition()];
    }
    first
        .snapshots
        .publish(publication(&config, 1, vec![definition()]))
        .unwrap();
    let late0 = page(&first);
    let late1 = page(&second);
    first.memory.publish(late0.clone()).unwrap();
    second.memory.publish(late1.clone()).unwrap();
    let adapter = caches::Adapter {
        node: node.clone(),
        listeners: first.prepared_listeners.clone(),
        capacity: config.limits.metadata_entries.get(),
    };
    assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    first.poll_retirement(&mut cx).unwrap();
    assert!(
        !node.retirement.quiescent().unwrap(),
        "worker zero cannot acknowledge worker one"
    );
    let (release, receive) = futures::channel::oneshot::channel::<()>();
    let memory = second.memory.clone();
    let late = late1.clone();
    crate::read::drivers::spawn(Box::pin(async move {
        receive.await.map_err(|_| Error::Cancelled)?;
        memory.publish(late)
    }))
    .unwrap();
    for _ in 0..4 {
        first.poll_retirement(&mut cx).unwrap();
        second.poll_retirement(&mut cx).unwrap();
    }
    assert!(!node.retirement.quiescent().unwrap());
    assert_eq!(
        first.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    assert!(first.memory.get(late0.plaintext.page()).unwrap().is_some());
    release.send(()).unwrap();
    // Avoid a real control poll here: drive the exact stage/acceptance handoff.
    first.control_task = Some(Box::pin(std::future::pending()));
    for _ in 0..8 {
        first.poll_retirement(&mut cx).unwrap();
        second.poll_retirement(&mut cx).unwrap();
    }
    assert!(node.retirement.quiescent().unwrap());
    let rejected = adapter.stage(&[]).unwrap();
    let mut invalid = publication(&config, 2, vec![]);
    invalid.members[0].peer_endpoint = "127.0.0.1:7444".into();
    assert!(matches!(
        first.snapshots.publish_staged(invalid, Some(rejected)),
        Err(Error::IncompatibleMembership)
    ));
    assert_eq!(
        first.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    assert!(first.memory.get(late0.plaintext.page()).unwrap().is_some());
    for _ in 0..4 {
        first.poll_retirement(&mut cx).unwrap();
        second.poll_retirement(&mut cx).unwrap();
    }
    first
        .snapshots
        .publish_staged(
            publication(&config, 2, vec![]),
            Some(adapter.stage(&[]).unwrap()),
        )
        .unwrap();
    first.poll_retirement(&mut cx).unwrap();
    assert!(first.retiring && second.retiring);
    assert!(second.memory.get(late1.plaintext.page()).unwrap().is_some());
    let until = Instant::now() + Duration::from_secs(5);
    while node.retirement.active().unwrap() {
        rt0.reactor.poll_budgeted(64).unwrap();
        rt1.reactor.poll_budgeted(64).unwrap();
        first.poll_retirement(&mut cx).unwrap();
        second.poll_retirement(&mut cx).unwrap();
        rt0.reactor.wait(Duration::from_millis(1)).unwrap();
        assert!(Instant::now() < until, "cache removal did not finish");
    }
    assert!(!node.retirement.active().unwrap());
    for (app, late) in [(&first, late0), (&second, late1)] {
        assert!(app.memory.get(late.plaintext.page()).unwrap().is_none());
        assert_eq!(app.memory.publish(late.clone()), Err(Error::Unavailable));
        let dirty = app
            .runtime
            .admission
            .reserve(
                Some(&definition().id),
                crate::model::limits::ResourceClass::DirtyCiphertext,
                19,
            )
            .unwrap();
        assert!(matches!(
            app.store.writer.enqueue(late.copy(), dirty),
            Err(Error::MissingKey)
        ));
    }
    assert!(second.peer_task.is_none() && second.diagnostic_task.is_none());
    // A removed UID cannot silently resurrect its tombstoned worker state.
    assert!(matches!(
        adapter.stage(&[definition()]),
        Err(Error::Unavailable)
    ));
    for (app, runtime, engine) in [
        (&mut first, &rt0, &mut crypto0),
        (&mut second, &rt1, &mut crypto1),
    ] {
        app.stop_admission().unwrap();
        if let Some(s) = &app.diagnostic_scope {
            s.cancel().unwrap();
        }
        app.peer_task.take();
        app.diagnostic_task.take();
        app.control_task.take();
        drive(runtime, engine, runtime.reactor.drain()).unwrap();
    }
}

#[test]
fn two_workers_start_from_real_control_and_checkpoint_one_complete_cut() {
    let mut fixture = Fixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0), WorkerId(1)], 64).unwrap());
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap();
    let ready = std::sync::Barrier::new(2);
    thread::scope(|threads| {
        for id in 0..2 {
            let (config, node, ready) = (&config, &node, &ready);
            threads.spawn(move || {
                let (mut app, runtime, mut engine) = local_worker(config, node, id);
                drive(
                    &runtime,
                    &mut engine,
                    app.start(&scope(Duration::from_secs(15)).unwrap()),
                )
                .unwrap();
                ready.wait();
                assert!(node.observations.health.ready());
                assert_eq!(app.peer_task.is_some(), id == 0);
                ready.wait();
                drive(
                    &runtime,
                    &mut engine,
                    app.drain(&scope(Duration::from_secs(5)).unwrap()),
                )
                .unwrap();
                drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
                drive(
                    &runtime,
                    &mut engine,
                    app.shutdown(&scope(Duration::from_secs(5)).unwrap()),
                )
                .unwrap();
            });
        }
    });
    let images = crate::store::recovery::read_candidates(&config.slab_directory).unwrap();
    assert_eq!(images.len(), 1);
    assert_eq!(images[0].1.shards.len(), 2);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 1);
}

struct Fixture {
    directory: PathBuf,
    stop: Arc<AtomicBool>,
    server: Option<thread::JoinHandle<()>>,
    config: Option<Config>,
    enrollments: Arc<AtomicUsize>,
    polls: Arc<AtomicUsize>,
}

#[test]
fn two_worker_real_control_retirement_and_checkpoint_cut() {
    use crate::{
        runtime::affinity::{EffectiveTopology, WorkerPair},
        store::checkpoint_format::CheckpointCodec,
    };
    let mut fixture = Fixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0), WorkerId(1)], 64).unwrap());
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap();
    let keys = Keyring::new(
        config.cluster.clone(),
        config.node.clone(),
        node.keys.clone(),
    );
    let limits = config.limits.clone();
    let app = Arc::new(Application {
        config: Arc::new(config),
        node: node.clone(),
        limits,
        fabric_ports: vec![],
    });
    let cpu = EffectiveTopology::discover().unwrap().cpus[0].clone();
    let plan = AffinityPlan {
        max_threads: 5,
        pairs: (0..2)
            .map(|id| WorkerPair {
                worker: WorkerId(id),
                io: cpu.clone(),
                crypto: cpu.clone(),
                nic: None,
            })
            .collect(),
    };
    let mut group = WorkerGroup::new(plan);
    group
        .start(app.clone(), &scope(Duration::from_secs(15)).unwrap())
        .unwrap();
    assert_eq!(node.prepared.load(Ordering::Acquire), 2);
    assert!(node.observations.health.ready());
    let cache = crate::model::identity::CacheId("33333333-3333-4333-8333-333333333333".into());
    let key = wire::CacheKeyRef {
        cache: cache.clone(),
        id: crate::model::envelope::KeyId([8; 16]),
        purpose: wire::CacheKeyPurpose::Page,
    };
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    keys.install(wire::KeyringBundle {
        schema_version: 1,
        cluster: app.config.cluster.clone(),
        generation: wire::BundleGeneration(2),
        peer_trust_roots: roots.clone(),
        cache_keys: vec![wire::CacheEncryptionKey {
            key: key.clone(),
            state: wire::CacheKeyState::Active,
            material: [21; 32],
        }],
    })
    .unwrap();
    let lease = keys.lease(Some(&cache), key.id, KeyPurpose::Page).unwrap();
    keys.install(wire::KeyringBundle {
        schema_version: 1,
        cluster: app.config.cluster.clone(),
        generation: wire::BundleGeneration(3),
        peer_trust_roots: roots,
        cache_keys: vec![],
    })
    .unwrap();
    let until = Instant::now() + Duration::from_secs(10);
    while !node.retirement.quiescent().unwrap() {
        assert!(
            Instant::now() < until,
            "both worker retirement fences did not arrive"
        );
        thread::sleep(Duration::from_millis(1));
    }
    assert_eq!(keys.pending_retirements().unwrap(), vec![key]);
    drop(lease);
    while node.retirement.active().unwrap() || !node.observations.health.ready() {
        assert!(
            Instant::now() < until,
            "two-worker retirement did not resume"
        );
        thread::sleep(Duration::from_millis(1));
    }
    assert!(keys.pending_retirements().unwrap().is_empty());
    let shutdown = scope(Duration::from_secs(10)).unwrap();
    group.drain(&shutdown).unwrap();
    group.shutdown(&shutdown).unwrap();
    group.join().unwrap();
    let bytes = std::fs::read(fixture.directory.join("slabs/checkpoint.0")).unwrap();
    let image = CheckpointCodec.decode(&bytes).unwrap();
    let mut workers: Vec<_> = image.shards.iter().map(|shard| shard.worker.0).collect();
    workers.sort_unstable();
    assert_eq!(workers, vec![0, 1]);
    assert!(!node.observations.health.ready());
}
impl Fixture {
    fn new() -> Self {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let directory = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "app-fixture-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(directory.join("secrets/epoch")).unwrap();
        let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        ca_params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let ca = ca_params.self_signed(&ca_key).unwrap();
        let server_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut params = rcgen::CertificateParams::new(vec!["127.0.0.1".into()]).unwrap();
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
        let cert = params.signed_by(&server_key, &ca, &ca_key).unwrap();
        let mut roots = rustls::RootCertStore::empty();
        roots.add(ca.der().clone()).unwrap();
        let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(roots),
            Arc::new(rustls::crypto::ring::default_provider()),
        )
        .allow_unauthenticated()
        .build()
        .unwrap();
        let tls = Arc::new(
            rustls::ServerConfig::builder_with_provider(Arc::new(
                rustls::crypto::ring::default_provider(),
            ))
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_client_cert_verifier(verifier)
            .with_single_cert(
                vec![cert.der().clone()],
                rustls::pki_types::PrivatePkcs8KeyDer::from(server_key.serialize_der()).into(),
            )
            .unwrap(),
        );
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let mut config = crate::test_support::cluster::config(false);
        config.control_endpoint = format!("https://{}", listener.local_addr().unwrap());
        config.trust_bundle = directory.join("trust.pem");
        config.service_account_token = directory.join("token");
        config.secret_directory = directory.join("secrets");
        config.identity_directory = directory.join("identity");
        config.slab_directory = directory.join("slabs");
        config.slab_bytes = 256 * 1024 * 1024;
        config.limits.range_window_pages = NonZeroUsize::new(2).unwrap();
        config.limits.connections_per_neighbor = NonZeroUsize::new(2).unwrap();
        config.limits.queue_entries = NonZeroUsize::new(64).unwrap();
        config.shutdown_timeout = Duration::from_secs(2);
        std::fs::write(&config.trust_bundle, ca.pem()).unwrap();
        std::fs::write(&config.service_account_token, b"fixture.token").unwrap();
        let bundle = wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(1),
            peer_trust_roots: vec![ca.der().to_vec()],
            cache_keys: vec![],
        };
        std::fs::write(
            directory.join("secrets/epoch/bundle.json"),
            wire::encode_bundle(&bundle).unwrap(),
        )
        .unwrap();
        symlink("epoch", directory.join("secrets/..data")).unwrap();
        let node = config.node.clone();
        let publication = wire::Publication {
            schema_version: 1,
            cluster: config.cluster.clone(),
            sequence: wire::PublicationSequence(1),
            membership_version: crate::model::identity::MembershipVersion(1),
            members: vec![crate::topology::membership::Member {
                node: node.clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
            caches: vec![],
        };
        let stop = Arc::new(AtomicBool::new(false));
        let stopping = stop.clone();
        let enrollments = Arc::new(AtomicUsize::new(0));
        let issued = enrollments.clone();
        let polls = Arc::new(AtomicUsize::new(0));
        let polled = polls.clone();
        let server = thread::spawn(move || {
            while !stopping.load(Ordering::Acquire) {
                let (socket, _) = match listener.accept() {
                    Ok(pair) => pair,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(1));
                        continue;
                    }
                    Err(e) => panic!("fixture accept: {e}"),
                };
                socket
                    .set_read_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                socket
                    .set_write_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                let mut stream = rustls::StreamOwned::new(
                    rustls::ServerConnection::new(tls.clone()).unwrap(),
                    socket,
                );
                let mut head = Vec::new();
                loop {
                    let mut byte = [0];
                    if stream.read_exact(&mut byte).is_err() {
                        break;
                    }
                    head.push(byte[0]);
                    if head.ends_with(b"\r\n\r\n") {
                        break;
                    }
                    assert!(head.len() <= 32768);
                }
                if !head.ends_with(b"\r\n\r\n") {
                    continue;
                }
                let head = String::from_utf8(head).unwrap();
                let length: usize = head
                    .lines()
                    .find_map(|line| {
                        line.split_once(':')
                            .filter(|(k, _)| k.eq_ignore_ascii_case("content-length"))
                            .map(|(_, v)| v.trim().parse().unwrap())
                    })
                    .unwrap_or(0);
                let mut body = vec![0; length];
                stream.read_exact(&mut body).unwrap();
                let (status, body) = if head.starts_with("POST ") {
                    assert!(head.contains("Authorization: Bearer fixture.token"));
                    assert!(stream.conn.peer_certificates().is_none());
                    let request = wire::decode_enrollment_request(&body).unwrap();
                    let der = rustls::pki_types::CertificateSigningRequestDer::from(
                        request.csr_der.clone(),
                    );
                    let mut csr = rcgen::CertificateSigningRequestParams::from_der(&der).unwrap();
                    csr.params.not_before =
                        (std::time::SystemTime::now() - Duration::from_secs(1)).into();
                    csr.params.not_after =
                        (std::time::SystemTime::now() + Duration::from_secs(86399)).into();
                    csr.params.subject_alt_names = vec![rcgen::SanType::URI(
                        format!("spiffe://{}/node/{}", request.cluster.0, node.0)
                            .try_into()
                            .unwrap(),
                    )];
                    csr.params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
                    csr.params.extended_key_usages =
                        vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
                    let cert = csr.signed_by(&ca, &ca_key).unwrap();
                    issued.fetch_add(1, Ordering::Release);
                    (
                        200,
                        wire::encode_enrollment_response(&wire::EnrollmentResponse {
                            schema_version: 1,
                            cluster: request.cluster,
                            node: node.clone(),
                            enrollment: request.enrollment,
                            certificate_chain: vec![cert.der().to_vec()],
                        })
                        .unwrap(),
                    )
                } else {
                    assert!(stream.conn.peer_certificates().is_some());
                    polled.fetch_add(1, Ordering::Release);
                    if head.lines().next().unwrap().contains("?after=1") {
                        (204, Vec::new())
                    } else {
                        (200, wire::encode_publication(&publication).unwrap())
                    }
                };
                let response = format!(
                    "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = stream
                    .write_all(response.as_bytes())
                    .and_then(|()| stream.write_all(&body))
                    .and_then(|()| stream.flush());
            }
        });
        Self {
            directory,
            stop,
            server: Some(server),
            config: Some(config),
            enrollments,
            polls,
        }
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(server) = self.server.take() {
            server.join().unwrap();
        }
        std::fs::remove_dir_all(&self.directory).unwrap();
    }
}
fn drive<T>(
    runtime: &WorkerRuntime,
    engine: &mut dyn CryptoService,
    future: Operation<'_, T>,
) -> Result<T> {
    let mut future = future;
    let until = Instant::now() + Duration::from_secs(15);
    loop {
        runtime.reactor.poll_budgeted(64)?;
        engine.poll_budgeted(64)?;
        runtime.crypto.poll_budgeted(64)?;
        if let Poll::Ready(result) = future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            return result;
        }
        assert!(
            Instant::now() < until,
            "application lifecycle did not progress"
        );
        runtime.reactor.wait(Duration::from_millis(1))?;
    }
}
#[test]
fn real_control_bootstrap_recovery_publication_readiness_and_shutdown() {
    let mut fixture = Fixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
    let startup = scope(Duration::from_secs(15)).unwrap();
    config.node = bootstrap(&config, &node, &config.limits, &startup).unwrap();
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 1);
    let admission = Rc::new(Admission::new(config.limits.clone()));
    let (io, engine) = crate::runtime::crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
    let runtime = WorkerRuntime {
        reactor: Rc::new(Reactor::new(admission.clone())),
        admission,
        crypto: Rc::new(crate::runtime::crypto::CryptoClient::new(io)),
    };
    let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine });
    let local = WorkerRuntime {
        reactor: runtime.reactor.clone(),
        admission: runtime.admission.clone(),
        crypto: runtime.crypto.clone(),
    };
    let mut worker = WorkerApplication::assemble(&config, &node, WorkerId(0), local).unwrap();
    worker.node = Some(node.clone());
    let startup = scope(Duration::from_secs(15)).unwrap();
    drive(&runtime, &mut engine, worker.start(&startup)).unwrap();
    assert!(node.observations.health.ready());
    assert!(
        fixture.polls.load(Ordering::Acquire) >= 2,
        "first publication waits for prepared resource retry"
    );
    assert_eq!(
        fixture.enrollments.load(Ordering::Acquire),
        1,
        "worker uses durably recovered identity"
    );
    for _ in 0..8 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
    }
    // Retire an omitted epoch through the actual serving loop. A held key lease
    // must keep material alive even after all worker/kernel/checkpoint barriers.
    let cache = crate::model::identity::CacheId("33333333-3333-4333-8333-333333333333".into());
    let reference = wire::CacheKeyRef {
        cache: cache.clone(),
        id: crate::model::envelope::KeyId([7; 16]),
        purpose: wire::CacheKeyPurpose::Page,
    };
    let roots = (*worker.keys.peer_trust_roots().unwrap()).clone();
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(2),
            peer_trust_roots: roots.clone(),
            cache_keys: vec![wire::CacheEncryptionKey {
                key: reference.clone(),
                state: wire::CacheKeyState::Active,
                material: [19; 32],
            }],
        })
        .unwrap();
    let lease = worker
        .keys
        .lease(Some(&cache), reference.id, KeyPurpose::Page)
        .unwrap();
    let page = crate::model::identity::PageId {
        version: crate::model::identity::ObjectVersion {
            object: crate::model::identity::ObjectId {
                cache: cache.clone(),
                key: crate::model::identity::CacheKey([4; 32]),
            },
            etag: crate::model::identity::StrongEtag::test_value("accepted"),
        },
        number: crate::model::identity::PageNumber(0),
    };
    let buffers = BufferPool::new(runtime.admission.clone());
    let plaintext = buffers
        .plaintext(
            runtime
                .admission
                .reserve(
                    Some(&cache),
                    crate::model::limits::ResourceClass::Plaintext,
                    8,
                )
                .unwrap(),
            8,
        )
        .unwrap();
    let ciphertext = runtime
        .admission
        .reserve(
            Some(&cache),
            crate::model::limits::ResourceClass::Ciphertext,
            24,
        )
        .unwrap();
    let accepted_scope = scope(Duration::from_secs(10)).unwrap();
    let mut accepted = runtime.crypto.execute(
        crate::runtime::crypto::CryptoInput::Encrypt {
            page,
            plaintext,
            ciphertext,
        },
        worker
            .keys
            .lease(Some(&cache), reference.id, KeyPurpose::Page)
            .unwrap(),
        &accepted_scope,
    );
    assert!(
        accepted
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert_eq!(runtime.crypto.outstanding(), 1);
    drop(accepted);
    accepted_scope.cancel().unwrap();
    let shard = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(worker.store.checkpoint.publish(vec![shard])).unwrap();
    worker.store.checkpoint.finish_snapshot();
    assert!(fixture.directory.join("slabs/checkpoint.0").is_file());
    let shard = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(worker.store.checkpoint.publish(vec![shard])).unwrap();
    worker.store.checkpoint.finish_snapshot();
    assert!(fixture.directory.join("slabs/checkpoint.1").is_file());
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(3),
            peer_trust_roots: roots,
            cache_keys: vec![],
        })
        .unwrap();
    let until = Instant::now() + Duration::from_secs(5);
    for _ in 0..8 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
    }
    assert!(
        fixture.directory.join("slabs/checkpoint.0").is_file(),
        "accepted crypto completion must precede checkpoint invalidation"
    );
    assert_eq!(runtime.crypto.outstanding(), 1);
    while worker.retirement_checkpoint.is_none() {
        engine.poll_budgeted(64).unwrap();
        runtime.crypto.poll_budgeted(64).unwrap();
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        assert!(
            Instant::now() < until,
            "checkpoint invalidation was not submitted"
        );
    }
    // Application polling alone cannot acknowledge an unconsumed filesystem CQE.
    let in_flight = runtime.reactor.in_flight();
    assert!(in_flight > 0);
    for _ in 0..4 {
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        assert!(worker.retirement_checkpoint.is_some());
        assert_eq!(runtime.reactor.in_flight(), in_flight);
        assert!(
            !crate::security::keyring::RetirementBarriers::fence(&*node.retirement, &reference)
                .unwrap()
        );
        assert_eq!(
            worker.keys.pending_retirements().unwrap(),
            vec![reference.clone()]
        );
    }
    while !crate::security::keyring::RetirementBarriers::fence(&*node.retirement, &reference)
        .unwrap()
    {
        engine.poll_budgeted(64).unwrap();
        runtime.crypto.poll_budgeted(64).unwrap();
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
        assert!(
            Instant::now() < until,
            "retirement checkpoint fence stalled"
        );
    }
    assert!(!fixture.directory.join("slabs/checkpoint.0").exists());
    assert!(!fixture.directory.join("slabs/checkpoint.1").exists());
    assert_eq!(
        worker.keys.pending_retirements().unwrap(),
        vec![reference.clone()]
    );
    let late = self::page(&worker);
    assert_eq!(worker.memory.publish(late.clone()), Err(Error::MissingKey));
    let dirty = runtime
        .admission
        .reserve(
            Some(&cache),
            crate::model::limits::ResourceClass::DirtyCiphertext,
            19,
        )
        .unwrap();
    assert!(matches!(
        worker.store.writer.enqueue(late.copy(), dirty),
        Err(Error::MissingKey)
    ));
    assert_eq!(runtime.crypto.outstanding(), 0);
    assert!(worker.retiring);
    drop(lease);
    while !worker.keys.pending_retirements().unwrap().is_empty() || worker.retiring {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
        assert!(
            Instant::now() < until,
            "retirement key destruction/resume stalled"
        );
    }
    runtime.reactor.poll_budgeted(64).unwrap();
    worker
        .poll_budgeted(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            64,
        )
        .unwrap();
    assert!(node.observations.health.ready());
    let shutdown = scope(Duration::from_secs(5)).unwrap();
    drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
    assert!(!node.observations.health.ready());
    drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
    assert_eq!(runtime.reactor.in_flight(), 0);
    assert_eq!(runtime.crypto.outstanding(), 0);
    assert!(fixture.directory.join("slabs/checkpoint.0").is_file());
}
