//! Actual SDK continuation through the production multi-peer read graph.
use super::*;
use crate::{client::request::RequestParser, read::serve::ReadService};
use std::{
    fs,
    os::{fd::AsRawFd, unix::net::UnixListener},
    process::{Child, Command},
    task::Poll,
};

struct Process(Child);
impl Drop for Process {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
#[ignore = "build the SDK fixture and run with --release"]
fn sdk_sliding_range_uses_fill_receive_capacity() {
    let root = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..");
    let output = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("target")
        .join(format!("stream-sdk-{}", std::process::id()));
    fs::create_dir_all(&output).unwrap();
    let overlay = output.join("overlay.json");
    fs::write(&overlay,serde_json::to_vec(&serde_json::json!({"Replace":{root.join("pkg/racersdk/production_stream_fixture_test.go").to_str().unwrap():root.join("cmd/racer-dataplane/tests/conformance/production_stream_fixture_test.go.txt")}})).unwrap()).unwrap();
    let binary = output.join("sdk.test");
    let status = Command::new("timeout")
        .arg("280s")
        .arg("go")
        .current_dir(&root)
        .arg("test")
        .arg("-overlay")
        .arg(&overlay)
        .arg("-c")
        .arg("-o")
        .arg(&binary)
        .arg("./pkg/racersdk")
        .status()
        .unwrap();
    assert!(status.success(), "SDK fixture build: {status}");
    for warm in [true, false] {
        run(warm, &binary);
    }
    fs::remove_dir_all(output).unwrap();
}
fn run(warm: bool, binary: &std::path::Path) {
    let (signers, discovery) =
        named_identities(&[A, B, C, "00000004-1111-4111-8111-111111111111"], 8192);
    for signer in &signers {
        for peer in &signers {
            signer
                .configure_authenticated_peer_challenge(
                    peer.node().clone(),
                    peer.challenge().unwrap(),
                )
                .unwrap();
        }
    }
    for (keys, _, _) in &discovery {
        let mut bundle: serde_json::Value =
            serde_json::from_slice(include_bytes!("../control/testdata/bundle.json")).unwrap();
        bundle["cluster"] = CLUSTER.into();
        bundle["generation"] = "2".into();
        for (i, key) in bundle["cache_keys"]
            .as_array_mut()
            .unwrap()
            .iter_mut()
            .enumerate()
        {
            key["cache"] = CACHE.into();
            key["material"] = base64::Engine::encode(
                &base64::engine::general_purpose::STANDARD,
                [i as u8 + 7; 32],
            )
            .into();
        }
        bundle["peer_trust_roots"] = serde_json::to_value(
            keys.peer_trust_roots()
                .unwrap()
                .iter()
                .map(|root| {
                    base64::Engine::encode(&base64::engine::general_purpose::STANDARD, root)
                })
                .collect::<Vec<_>>(),
        )
        .unwrap();
        keys.install(
            crate::control::wire::decode_bundle(&serde_json::to_vec(&bundle).unwrap()).unwrap(),
        )
        .unwrap();
    }
    let listeners: Vec<_> = (0..4)
        .map(|_| {
            let l = TcpListener::bind("127.0.0.1:0").unwrap();
            l.set_nonblocking(true).unwrap();
            l
        })
        .collect();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            signers
                .iter()
                .zip(&listeners)
                .map(|(s, l)| Member {
                    node: s.node().clone(),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: l.local_addr().unwrap().to_string(),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let placement = Placement::new(256);
    let mut bytes = HashMap::new();
    let mut descriptors = Vec::new();
    // Four-member placement excludes ingress for every page. Thus continuation
    // must exercise requester Fill plus peer receive, never a local origin escape.
    for layer in 0..8u8 {
        let length: usize = [
            59_757_056, 68_935_168, 65_509_376, 59_679_232, 71_126_528, 79_482_880, 75_599_872,
            62_860_288,
        ][layer as usize];
        let key = (0..100_000u32)
            .find_map(|n| {
                let mut key = [0; 32];
                key[..4].copy_from_slice(&n.to_le_bytes());
                key[4] = layer;
                let key = CacheKey(key);
                (0..length.div_ceil(P))
                    .all(|number| {
                        !placement
                            .rank(membership.clone(), &object(key), PageNumber(number as u64))
                            .unwrap()
                            .ordered
                            .contains(signers[0].node())
                    })
                    .then_some(key)
            })
            .expect("remote-ranked layer");
        let body = vec![layer; length];
        descriptors.push(serde_json::json!({"raw_key":key.0.to_vec(), "size":length,"digest":format!("sha256:{:x}",Sha256::digest(&body))}));
        bytes.insert(key, body);
    }
    let config = br#"{"architecture":"amd64","os":"linux"}"#.to_vec();
    let config_key = CacheKey(Sha256::digest(&config).into());
    let config_descriptor = serde_json::json!({"raw_key":config_key.0.to_vec(),"digest":format!("sha256:{:x}",Sha256::digest(&config)),"size":config.len()});
    let manifest = serde_json::to_vec(
        &serde_json::json!({"schemaVersion":2,"config":config_descriptor,"layers":descriptors}),
    )
    .unwrap();
    let manifest_key = CacheKey(Sha256::digest(&manifest).into());
    let manifest_descriptor = serde_json::json!({"raw_key":manifest_key.0.to_vec(),"digest":format!("sha256:{:x}",Sha256::digest(&manifest)),"size":manifest.len()});
    bytes.insert(config_key, config);
    bytes.insert(manifest_key, manifest);
    let data = Rc::new(Data {
        metadata_enabled: true,
        bytes,
        calls: RefCell::new(HashMap::new()),
    });
    // Seven full-page charges fit inside the fleet's 128 MiB worker quota.
    // Four two-page windows must make progress without an eighth full-page charge.
    let nodes: Vec<_> = (0..4)
        .map(|i| {
            build_node(
                i,
                membership.clone(),
                signers[i].clone(),
                &discovery[i],
                data.clone(),
                if i == 0 { 6 } else { 63 },
            )
        })
        .collect();
    let mut endpoints: Vec<_> = nodes
        .iter()
        .map(|node| {
            node.owners
                .install(WorkerId(0), node.coordinator.clone())
                .unwrap()
        })
        .collect();
    let scope = RequestScope::new(
        RequestId([77; 16]),
        Instant::now() + Duration::from_secs(100),
    )
    .unwrap();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut drive = |work: std::pin::Pin<Box<dyn Future<Output = ()> + '_>>| {
        let mut work = work;
        loop {
            if work.as_mut().poll(&mut cx).is_ready() {
                break;
            }
            scope.check().unwrap();
            for (node, endpoint) in nodes.iter().zip(&mut endpoints) {
                node.reactor.poll_budgeted(256).unwrap();
                node.engine.borrow_mut().poll_budgeted(64).unwrap();
                node.crypto.poll_budgeted(64).unwrap();
                endpoint.poll(&mut cx, 64).unwrap();
                node.flights.poll_with_context(&mut cx, 64).unwrap();
            }
            crate::read::drivers::poll(&mut cx, 64);
            nodes[0].reactor.wait(Duration::from_micros(100)).unwrap();
        }
    };
    // Seed real encrypted candidate caches through elected local Fill. The origin
    // adapter only supplies bytes; all requester/peer acquisition remains real.
    if warm {
        drive(Box::pin(async {
            for key in data.bytes.keys() {
                for number in 0..data.bytes[key].len().div_ceil(P) {
                    let page = page(*key, number as u64);
                    let rank = placement
                        .rank(membership.clone(), &page.version.object, page.number)
                        .unwrap();
                    let i = signers
                        .iter()
                        .position(|s| s.node() == &rank.ordered[0])
                        .unwrap();
                    let ctx = context(*key);
                    let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
                    drop(
                        nodes[i]
                            .fill
                            .acquire(page, membership.clone(), &ctx, &scope, &mut budget)
                            .await
                            .unwrap(),
                    );
                    nodes[i].writer.discard_unsubmitted();
                }
            }
        }));
    }
    let output = nodes[0].directory.join("sdk");
    fs::create_dir(&output).unwrap();
    let directory = fs::File::open(&output).unwrap();
    let socket = format!(
        "/proc/{}/fd/{}/socket",
        std::process::id(),
        directory.as_raw_fd()
    );
    let listener = UnixListener::bind(&socket).unwrap();
    listener.set_nonblocking(true).unwrap();
    let fixture = output.join("manifest.json");
    fs::write(&fixture, serde_json::to_vec(&manifest_descriptor).unwrap()).unwrap();
    let mut child = Process(
        Command::new(binary)
            .args([
                "-test.run=^TestProductionStreamFixture$",
                "-test.timeout=90s",
                "-test.v",
            ])
            .env("RACER_STREAM_SOCKET", socket)
            .env("RACER_STREAM_LAYERS", fixture)
            .spawn()
            .unwrap(),
    );
    let servers = async {
        let mut all = FuturesUnordered::new();
        for (node, listener) in nodes.iter().zip(&listeners) {
            let fd = Rc::new(OwnedFd::from(listener.try_clone().unwrap()));
            let scope = &scope;
            all.push(async move {
                let mut active=FuturesUnordered::new();
                loop {
                    let accept=node.reactor.accept(fd.clone(),scope); futures::pin_mut!(accept);
                    let accepted=loop {
                        if active.is_empty() {break accept.await;}
                        use futures::FutureExt;
                        futures::select_biased! {_ = active.next().fuse()=>{}, result=accept.as_mut().fuse()=>break result}
                    }?;
                    let mut connection=ConnectionLease::from_accepted(accepted,&node.admission)?;
                    active.push(async move {loop {connection=node.server.serve_connection(connection,scope).await?; if !connection.is_reusable(){return Ok::<(),Error>(());}}});
                }
                #[allow(unreachable_code)] Ok::<(),Error>(())
            });
        }
        all.next().await.unwrap()
    };
    let client_server = async {
        let node = &nodes[0];
        let mut active = FuturesUnordered::new();
        loop {
            while let Ok((stream, _)) = listener.accept() {
                let scope = &scope;
                active.push(async move {
                    let mut connection =
                        ConnectionLease::from_accepted(stream.into(), &node.admission)?;
                    loop {
                        let request_scope =
                            RequestScope::new(RequestId(rand_id()), scope.deadline.0)?;
                        let received = node
                            .client_io
                            .receive_head(connection, &request_scope)
                            .await?;
                        let request = RequestParser::new(32768)
                            .parse(&CacheId(CACHE.into()), received.value)?;
                        let kind = request.kind.clone();
                        let response = node
                            .coordinator
                            .read(request, &request_scope)
                            .await
                            .inspect_err(|e| eprintln!("read {kind:?}: {e:?}"))?;
                        node.responses.validate(&kind, &response)?;
                        connection = node
                            .responses
                            .send(received.connection, response, &request_scope)
                            .await
                            .inspect_err(|e| eprintln!("send {kind:?}: {e:?}"))?;
                    }
                    #[allow(unreachable_code)]
                    Ok::<(), Error>(())
                });
            }
            let finished = std::future::poll_fn(|cx| {
                while let Poll::Ready(Some(result)) = active.poll_next_unpin(cx) {
                    eprintln!("client connection: {result:?}");
                }
                if let Some(status) = child.0.try_wait().unwrap() {
                    assert!(status.success(), "SDK failed: {status}");
                    return Poll::Ready(true);
                }
                Poll::Ready(false)
            })
            .await;
            if finished {
                break;
            }
            let mut yielded = false;
            std::future::poll_fn(|_| {
                if yielded {
                    Poll::Ready(())
                } else {
                    yielded = true;
                    Poll::Pending
                }
            })
            .await;
        }
    };
    drive(Box::pin(async {
        futures::pin_mut!(servers, client_server);
        match futures::future::select(client_server, servers).await {
            futures::future::Either::Left(_) => {}
            futures::future::Either::Right((result, _)) => panic!("peer server: {result:?}"),
        }
    }));
    for node in &nodes {
        node.pool.close();
        node.writer.discard_unsubmitted();
        node.memory.evict_idle(usize::MAX).unwrap();
    }
    drive(Box::pin(async {
        for node in &nodes {
            node.reactor.drain().await.unwrap();
        }
    }));
    for node in &nodes {
        node.writer.discard_unsubmitted();
        node.memory.evict_idle(usize::MAX).unwrap();
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::Ciphertext,
            ResourceClass::DirtyCiphertext,
            ResourceClass::Relay,
            ResourceClass::Connection,
        ] {
            assert_eq!(node.admission.used(class), 0, "{class:?}");
        }
    }
    eprintln!(
        "full SDK image: manifest, config, eight layers / 542950400 layer bytes; warm={warm}"
    );
}
fn rand_id() -> [u8; 16] {
    let mut id = [0; 16];
    getrandom::getrandom(&mut id).unwrap();
    id
}
