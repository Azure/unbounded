//! Generated traffic through the assembled production graph on hostless descriptors.
//! The oracle owns immutable origin versions, never consults placement or cache data.

use crate::admission::ResourceClass;
use crate::app::*;
use crate::lifecycle::Service;
use crate::model::PAGE_BYTES;
use crate::model::*;
use racer_crypto::identity::PendingIdentity;
use racer_crypto::identity::SigningIdentity;
use racer_io::ring::Descriptor;
use racer_io::ring::SocketAddress;
use racer_io::sim::Fault;
use racer_io::sim::SimOs;
use racer_wire::control as wire;
use racer_wire::control::CacheDefinition;
use racer_wire::control::CacheId;
use racer_wire::control::MembershipVersion;
use sha2::Digest;
use sha2::Sha256;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::collections::VecDeque;
use std::num::NonZeroUsize;

/// Assembles one worker graph on `env` through `WorkerApplication::assemble`.
pub(crate) fn local_worker_with_fabric(
    env: &racer_io::Env,
    config: &Config,
    node: &Arc<NodeState>,
    id: u16,
    discovered_nics: Vec<racer_wire::control::RailMapping>,
) -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    let worker = WorkerId(id);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let (io, engine) = crate::security::pair(worker, 0, config.limits.queue_entries);
    let runtime = WorkerRuntime {
        reactor: Rc::new(Reactor::new(admission.clone(), env)),
        admission,
        crypto: Rc::new(crate::security::CryptoClient::new(io)),
    };
    let local = WorkerRuntime {
        reactor: runtime.reactor.clone(),
        admission: runtime.admission.clone(),
        crypto: runtime.crypto.clone(),
    };
    let app =
        WorkerApplication::assemble(config, node.clone(), worker, local, discovered_nics).unwrap();
    (
        app,
        runtime,
        PageCryptoEngine::new(CryptoRuntime {
            port: engine,
            ctx: env.ctx.clone(),
        }),
    )
}

/// A one-member publication carrying `caches` at `sequence`.
pub(crate) fn publication(
    config: &Config,
    sequence: u64,
    caches: Vec<racer_wire::control::CacheDefinition>,
) -> wire::Publication {
    wire::Publication {
        pools: Vec::new(),
        schema_version: racer_wire::control::SCHEMA_VERSION,
        cluster: config.cluster.clone(),
        sequence: wire::PublicationSequence(sequence),
        // Pools cover the caches, so each catalog takes its own version.
        membership_version: racer_wire::control::MembershipVersion(sequence),
        members: vec![racer_wire::control::Member {
            node: config.node.clone(),
            shares: std::num::NonZeroU32::new(1).unwrap(),
            peer_endpoint: "127.0.0.1:7443".into(),
            rails: vec![],
            site: String::new(),
        }],
        caches,
        page_size: racer_wire::object::PageSize::DEFAULT,
    }
    .with_default_pool()
}

/// Base node configuration shared by the simulation and unit fixtures.
pub mod cluster {
    use crate::config::Config;
    use crate::config::Limits;
    use racer_wire::control::ClusterId;
    use racer_wire::control::NodeId;
    use std::num::NonZeroU32;
    use std::num::NonZeroUsize;
    use std::time::Duration;

    pub fn config(enable_rdma: bool) -> Config {
        let count = NonZeroUsize::new(16).unwrap();
        let bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
        Config {
            spill: Default::default(),
            prefetch: Default::default(),
            send_tag_pair: None,
            page_hedge: Default::default(),
            replica_selection: Default::default(),
            peer_admission: Default::default(),
            peer_receive: Default::default(),
            shares: NonZeroU32::new(4).unwrap(),
            disk_page_entries: NonZeroUsize::new(65536).unwrap(),
            admission_mode: crate::config::AdmissionMode::SecondSight,
            admission_history_bytes: NonZeroUsize::new(4 * 1024 * 1024).unwrap(),
            admission_period: Duration::from_secs(60),
            checkpoint_bytes: NonZeroUsize::new(64 * 1024 * 1024).unwrap(),
            device_directory: "/host/dev".into(),
            device_busy_wait: Duration::ZERO,
            cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
            node: NodeId("00000000-0000-4000-8000-000000000002".into()),
            max_threads: 2,
            io_workers: None,
            crypto_helpers: None,
            allow_smt: false,
            opaque_relay: false,
            peer_tcp_nodelay: false,
            memory_pressure: true,
            pprof_enabled: false,
            enable_rdma,
            enable_gpu: crate::config::GpuMode::Disabled,
            gpu_staging_bytes: crate::config::DEFAULT_GPU_STAGING_BYTES,
            gpu_landing_bytes: crate::config::DEFAULT_GPU_LANDING_BYTES,
            gpu_pci_p2pdma: false,
            gpu_peer: true,
            control_endpoint: Some("https://control.invalid".into()),
            managed_control: None,
            enrollment_directory: None,
            peer_listen: "127.0.0.1:0".parse().unwrap(),
            diagnostics_listen: "127.0.0.1:0".parse().unwrap(),
            trust_bundle: "unused/ca".into(),
            service_account_token: "unused/token".into(),
            identity_directory: "unused/identity".into(),
            slab_directory: "unused/slabs".into(),
            slab_bytes: 1024 * 1024 * 1024,
            segment_bytes: 64 * 1024 * 1024,
            free_segment_reserve: 2,
            origin_connections_per_cache: NonZeroUsize::new(8).unwrap(),
            request_timeout: Duration::from_secs(30),
            peer_attempt_timeout: Duration::from_secs(30),
            reader_stall_timeout: Duration::from_secs(10),
            shutdown_timeout: Duration::from_secs(30),
            limits: Limits {
                plaintext_bytes: bytes,
                ciphertext_bytes: bytes,
                dirty_bytes: bytes,
                registered_bytes: bytes,
                request_context_bytes: bytes,
                flights: count,
                waiters_per_flight: count,
                queue_entries: count,
                connections_per_neighbor: count,
                client_connections: count,
                peer_connections: count,
                outbound_connections: count,
                pipes: count,
                range_window_pages: count,
                header_bytes: NonZeroUsize::new(16 * 1024).unwrap(),
                placement_cache_bytes: NonZeroUsize::new(16 * crate::topology::RANKING_BYTES)
                    .unwrap(),
                path_cache_bytes: NonZeroUsize::new(8 * 1024 * 1024).unwrap(),
                active_path_searches: NonZeroUsize::new(8).unwrap(),
                cached_paths: count,
                retained_snapshots: count,
                metadata_entries: count,
                relay_transfers: count,
            },
            scaled_limits: Default::default(),
            origin: Default::default(),
            authority: None,
        }
    }
}

/// Re-encodes a signed head so tests can mutate a private copy.
pub(crate) fn clone_head(
    head: &crate::peer::protocol::SignedHead,
) -> crate::peer::protocol::SignedHead {
    use crate::http::Codec;
    use crate::peer::protocol;
    use crate::peer::protocol::SignedHead;
    let codec = Codec::new(protocol::MAX_HEAD);
    let encoded = codec.encode_head(&head.head).unwrap();
    SignedHead {
        head: codec.decode_head(&encoded).unwrap().unwrap().0,
        signature: head.signature.clone(),
        chain: head.chain.clone(),
    }
}

/// Issues and accepts a node signing identity under the harness CA.
fn signing_identity(
    pending: PendingIdentity,
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    cluster: racer_wire::control::ClusterId,
    node: racer_wire::control::NodeId,
) -> Arc<SigningIdentity> {
    let (pending, chain) = racer_crypto::identity::test_util::issue_pending(
        pending,
        ca,
        ca_key,
        &cluster,
        &node,
        |_| {},
    );
    Arc::new(
        pending
            .accept(
                cluster,
                node,
                chain,
                &[ca.der().to_vec()],
                &racer_crypto::identity::WallClock::host(),
            )
            .unwrap(),
    )
}

/// Real wakers for every poll the harness makes. Wakes are recorded rather
/// than ignored, so [`Harness::settle`] can fail a run whose pending work has
/// no wake source left (no completion, no in-flight operation, no wake).
mod wake {
    use std::cell::RefCell;
    use std::sync::Arc;
    use std::sync::atomic::AtomicBool;
    use std::sync::atomic::Ordering;
    use std::task::Waker;

    #[derive(Default)]
    struct Flag(AtomicBool);

    impl std::task::Wake for Flag {
        fn wake(self: Arc<Self>) {
            self.0.store(true, Ordering::Release);
        }

        fn wake_by_ref(self: &Arc<Self>) {
            self.0.store(true, Ordering::Release);
        }
    }

    thread_local! {
        static FLAG: RefCell<Option<Arc<Flag>>> = const { RefCell::new(None) };
    }

    fn flag() -> Arc<Flag> {
        FLAG.with(|flag| {
            flag.borrow_mut()
                .get_or_insert_with(Default::default)
                .clone()
        })
    }

    /// The harness thread's recording waker.
    pub(super) fn waker() -> Waker {
        Waker::from(flag())
    }

    /// Whether any task was woken since the last call; clears the record.
    pub(super) fn take() -> bool {
        flag().0.swap(false, Ordering::AcqRel)
    }
}

/// Seeds of the default generated corpus; together they meet every coverage
/// obligation in `coverage_misses`.
const DEFAULT_SEEDS: [u64; 3] = [1, 7, 42];

/// Result of one app scenario run.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Run {
    /// Digest of the run's replay trace; equal inputs give equal digests.
    pub digest: [u8; 32],
}

/// App scenario names accepted by [`run`]: `generated` and
/// `generated-native` replay one seed through generated traffic, churn and
/// faults; `corpus` and `corpus-native` run the default seed set with its
/// coverage obligations; the rest are targeted regressions on fixed seeds.
pub const SCENARIOS: &[&str] = &[
    "generated",
    "generated-native",
    "corpus",
    "corpus-native",
    "default-grace-staggered-checkpoint",
    "origin-recovery-before-healthy-reads",
    "native-faults-warmed-peers",
    "refused-peer-connect-fallback",
    "shutdown-checkpoint-async-io",
    "worker-subscriptions-contend",
    "completed-peer-dispatches-cancellation",
    "healthy-relayed-page-reads",
    "healthy-relayed-page-reads-opaque",
    "listeners-survive-queue-pressure",
    "concurrent-relayed-layers",
    "concurrent-relayed-layers-opaque",
];

/// Runs app scenario `name` (see [`SCENARIOS`]) with `seed` and up to
/// `steps` generated actions (generated scenarios only; 0 means one full
/// action cycle). Every safety invariant (oracle bytes, 412/502 contract,
/// resource bounds, release at quiescence, stranded work) is a panic, which
/// becomes `Err` carrying the panic message.
pub fn run(name: &str, seed: u64, steps: usize) -> std::result::Result<Run, String> {
    let steps = if steps == 0 {
        WEIGHTED_ACTIONS.len()
    } else {
        steps.min(512)
    };
    let outcome = std::panic::catch_unwind(|| {
        let fixed = |f: fn()| {
            f();
            Sha256::digest(name.as_bytes()).into()
        };
        match name {
            "generated" => replay(seed, steps, false).trace.digest(),
            "generated-native" => replay(seed, steps, true).trace.digest(),
            "corpus" => run_corpus(false, &DEFAULT_SEEDS, WEIGHTED_ACTIONS.len(), true),
            "corpus-native" => run_corpus(true, &DEFAULT_SEEDS, WEIGHTED_ACTIONS.len(), true),
            "default-grace-staggered-checkpoint" => {
                fixed(default_grace_staggered_nodes_and_periodic_checkpoint_traffic)
            }
            "origin-recovery-before-healthy-reads" => {
                fixed(dst_origin_recovery_completes_before_concurrent_healthy_reads)
            }
            "native-faults-warmed-peers" => {
                fixed(dst_native_faults_reach_warmed_peers_in_small_topologies)
            }
            "refused-peer-connect-fallback" => {
                fixed(dst_refused_peer_connect_preserves_candidate_fallback)
            }
            "shutdown-checkpoint-async-io" => {
                fixed(dst_shutdown_checkpoint_uses_async_io_and_releases_all_shards)
            }
            "worker-subscriptions-contend" => fixed(
                scenarios::worker_subscriptions_contend_across_servers_and_recover_after_release,
            ),
            "completed-peer-dispatches-cancellation" => {
                fixed(scenarios::completed_peer_dispatches_do_not_exhaust_worker_cancellation)
            }
            "healthy-relayed-page-reads" => fixed(scenarios::healthy_relayed_page_reads),
            "healthy-relayed-page-reads-opaque" => {
                fixed(scenarios::healthy_relayed_page_reads_opaque_opt_in)
            }
            "listeners-survive-queue-pressure" => {
                fixed(scenarios::listeners_survive_repeated_reactor_queue_pressure)
            }
            "concurrent-relayed-layers" => {
                fixed(scenarios::concurrent_relayed_layers_diagnose_receive_pressure_and_recover)
            }
            "concurrent-relayed-layers-opaque" => {
                fixed(scenarios::concurrent_relayed_layers_opaque_opt_in_pressure_and_recovery)
            }
            other => panic!("unknown app scenario {other}"),
        }
    });
    outcome.map(|digest| Run { digest }).map_err(|panic| {
        panic
            .downcast_ref::<String>()
            .cloned()
            .or_else(|| panic.downcast_ref::<&str>().map(|s| (*s).to_string()))
            .unwrap_or_else(|| "non-string panic".into())
    })
}

const MAX_NODES: usize = 32;
const MAX_TURNS: usize = 100_000;
/// Consecutive settle turns with unfinished work but no completion, no
/// in-flight operation and no wake before the run fails as stranded.
const STRANDED_TURNS: usize = 2_000;
/// Subscription fields every simulated reader sends with its POST.
// Keeps the removed Racer-Ordered header as a compatibility check: the
// dataplane ignores it.
const SUBSCRIPTION_FIELDS: &str = "Content-Length: 0\r\nRacer-Page-Credits: 64\r\nRacer-Byte-Credits: 1073741824\r\nRacer-Ordered: 1\r\n";

/// Seeded role entropy replays for the same reads and stays separate between
/// role streams.
#[test]
fn racer_entropy_replay_bytes_are_stable() {
    let clock = racer_io::SimClock::new(7);
    let role = clock.ctx(11);
    let mut bytes = [0; 32];
    for chunk in bytes.chunks_mut(3) {
        role.rng.fill(chunk).unwrap();
    }
    let replay_role = clock.ctx(11);
    let mut replay = [0; 32];
    for chunk in replay.chunks_mut(3) {
        replay_role.rng.fill(chunk).unwrap();
    }
    assert_eq!(bytes, replay);
    let mut neighbor = [0; 32];
    clock.ctx(12).rng.fill(&mut neighbor).unwrap();
    assert_ne!(bytes, neighbor);
}

mod faults {
    //! Fault injection and recovery actions for the generated traffic harness.
    use super::*;

    impl Harness {
        pub(super) fn recover_origin(&mut self) {
            // Quiescence alone does not close a circuit. Neither does elapsed backoff:
            // concurrent healthy requests would compete for its exclusive half-open
            // probe. Complete real, serial origin I/O on every worker before resuming.
            self.settle();
            self.clock.advance(Duration::from_secs(26));
            for node in 0..self.nodes.len() {
                for worker in 0..self.nodes[node].workers.len() {
                    let object = (8..16384)
                        .find(|&object| {
                            let id = self.object_id(object);
                            self.nodes[node].workers[0]
                                .app
                                .directory
                                .metadata_owner(&id)
                                .unwrap()
                                == WorkerId(worker as u16)
                                && self.ranked_nodes(&id)[0] == node
                        })
                        .expect("bounded recovery key search");
                    // A new version prevents a local or peer metadata hit from
                    // impersonating origin recovery. HEAD has no concurrent page fill.
                    self.update(object);
                    let calls = self.catalog.borrow().calls;
                    let probe = self.request_on(object, true, true, node);
                    self.exchange(probe, false);
                    assert_eq!(
                        self.catalog.borrow().calls,
                        calls + 1,
                        "recovery skipped origin"
                    );
                }
            }
            self.coverage.action("origin-recovered");
        }

        pub(super) fn origin_fault(&mut self) {
            // Prior malformed replies now open endpoint circuits. Advance beyond the
            // maximum backoff before requiring this distinct fault to reach origin.
            self.clock.advance(Duration::from_secs(26));
            self.settle();
            if self.origin_faults.is_empty() {
                self.origin_faults.extend([
                    OriginFault::Reject,
                    OriginFault::Forbidden,
                    OriginFault::DuplicateLength,
                    OriginFault::Truncate,
                    OriginFault::WrongEtag,
                ]);
            }
            let index = self.rng.pick(self.origin_faults.len());
            let fault = self.origin_faults.swap_remove(index);
            self.catalog.borrow_mut().fault = Some(fault);
            self.update(6);
            let client = self.request(6, false, false);
            self.exchange(client, true);
            assert!(
                self.catalog.borrow().fault.is_none(),
                "origin fault not consumed"
            );
            self.coverage.action("origin-fault");
            self.recover_origin();
        }

        pub(super) fn malformed_client(&mut self) {
            let mut client = self.request(2, false, false);
            let (method, fields, status) = match self.rng.pick(3) {
                0 => ("POST", "Host: racer\r\nHost: racer\r\n", 400),
                1 => ("GET", "Host: racer\r\n", 405),
                _ => ("POST", "Host: racer\r\nContent-Length: 1\r\n", 400),
            };
            client.request =
                format!("{method} /v1/objects/{} HTTP/1.1\r\n{fields}\r\n", key(2)).into_bytes();
            client.expected_status = Some(status);
            self.exchange(client, false);
            self.coverage.action("malformed-client");
        }

        fn raw_peer(&mut self, node: usize, request: Vec<u8>) -> Vec<u8> {
            self.coverage.trace.record(format!(
                "peer-request:{}:{:x}",
                self.nodes[node].id,
                Sha256::digest(&request)
            ));
            let fd = self
                .sim
                .connect(SocketAddress::Inet(self.nodes[node].config.peer_listen))
                .unwrap();
            let mut sent = 0;
            let mut response = Vec::new();
            let mut finished = false;
            for _ in 0..MAX_TURNS {
                self.tick();
                if sent < request.len() {
                    match handle(&fd).send(&request[sent..]) {
                        Ok(n) => sent += n,
                        Err(e) if would_block(&e) => (),
                        Err(_) => {
                            finished = true;
                            break;
                        }
                    }
                }
                let mut bytes = [0; 32768];
                match handle(&fd).recv(&mut bytes) {
                    Ok(0) => {
                        finished = true;
                        break;
                    }
                    Ok(n) => response.extend_from_slice(&bytes[..n]),
                    Err(e) if would_block(&e) => (),
                    Err(_) => {
                        finished = true;
                        break;
                    }
                }
                if response.windows(4).any(|w| w == b"\r\n\r\n") {
                    let (_, h, end) = headers(&response);
                    if response.len() >= end + h["content-length"].parse::<usize>().unwrap() {
                        finished = true;
                        break;
                    }
                }
            }
            drop(fd);
            assert!(
                finished,
                "peer probe timed out without an observed rejection or response"
            );
            self.coverage.trace.record(format!(
                "peer-response:{}:{:x}",
                self.nodes[node].id,
                Sha256::digest(&response)
            ));
            response
        }

        pub(super) fn peer_security(&mut self, env: &racer_io::Env) {
            use crate::http::Codec;
            use crate::peer::protocol as p;
            use crate::peer::protocol::encode_envelope;
            use http1::MessageHead;
            use http1::StartLine;
            let receiver = self.rng.pick(self.nodes.len());
            let sender = (receiver + 1) % self.nodes.len();
            let keys = self.nodes[sender].workers[0].app.keys.clone();
            let certificates = Rc::new(Certificates::new(
                self.nodes[sender].config.cluster.clone(),
                keys.clone(),
            ));
            let signatures = Signatures::new(keys, certificates.clone(), env.ctx.clone());
            let peer = self.nodes[receiver].config.node.clone();
            let mut head = MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1/handshake".into(),
                },
                headers: vec![],
            };
            p::push(&mut head, "content-length", 0);
            p::push(&mut head, "racer-kind", "handshake");
            p::push(
                &mut head,
                "racer-wire-version",
                crate::peer::protocol::VERSION,
            );
            p::push(&mut head, "racer-membership", self.generation);
            p::push(&mut head, "racer-receiver", &peer.0);
            let signed = signatures.sign(head).unwrap();
            let envelope = crate::peer::forwarding::ForwardedHead {
                original: Arc::new(signed),
                hops: vec![],
            };
            let wire = encode_envelope(&envelope, false, 0).unwrap();
            let bytes = Codec::new(32768).encode_head(&wire).unwrap();
            if self.security_faults.is_empty() {
                self.security_faults.extend([true, false]);
            }
            let selected = self.rng.pick(self.security_faults.len());
            if self.security_faults.swap_remove(selected) {
                // A retained signed proof on a new socket has no live session. Both
                // attempts must fail, including after receiver restart.
                let valid = self.raw_peer(receiver, bytes.clone());
                assert!(
                    !valid.starts_with(b"HTTP/1.1 200"),
                    "sessionless signed proof accepted"
                );
                let replay = self.raw_peer(receiver, bytes);
                assert!(
                    !replay.starts_with(b"HTTP/1.1 200"),
                    "replayed signed request accepted"
                );
                self.coverage.action("peer-replay");
            } else {
                // The envelope wraps the signed original in a base64 field. Mutate the
                // signed signature before encoding so the outer HTTP remains legal.
                let original = &envelope.original;
                let mut signed = clone_head(original);
                let signature = signed
                    .head
                    .headers
                    .iter_mut()
                    .find(|h| h.name.eq_ignore_ascii_case("signature"))
                    .unwrap();
                let at = signature.value.iter().position(|b| *b == b':').unwrap() + 1;
                signature.value[at] = if signature.value[at] == b'A' {
                    b'B'
                } else {
                    b'A'
                };
                signed.signature[0] ^= 1;
                let wire = encode_envelope(
                    &crate::peer::forwarding::ForwardedHead {
                        original: Arc::new(signed),
                        hops: vec![],
                    },
                    false,
                    0,
                )
                .unwrap();
                let bytes = Codec::new(32768).encode_head(&wire).unwrap();
                let response = self.raw_peer(receiver, bytes);
                assert!(
                    !response.starts_with(b"HTTP/1.1 200"),
                    "corrupted signature accepted"
                );
                self.coverage.action("peer-signature-corruption");
            }
        }

        pub(super) fn disk_corruption(&mut self) {
            use racer_io::sim::DiskState;
            self.settle();
            let entries: Vec<_> = self
                .nodes
                .iter()
                .enumerate()
                .flat_map(|(n, node)| {
                    node.workers
                        .iter()
                        .enumerate()
                        .flat_map(move |(w, worker)| {
                            worker
                                .app
                                .store
                                .writer
                                .index()
                                .snapshot()
                                .unwrap()
                                .entries
                                .into_iter()
                                .map(move |entry| (n, w, entry))
                        })
                })
                .filter(|(node, worker, (page, entry))| {
                    let object =
                        usize::from_str_radix(&page.version.object.key.to_hex(), 16).unwrap();
                    object > 1
                        && self.nodes[*node].workers[*worker]
                            .app
                            .keys
                            .lease(
                                Some(&page.version.object.cache),
                                entry.key_id,
                                KeyPurpose::Page,
                            )
                            .is_ok()
                        && self.nodes[*node].workers[*worker]
                            .app
                            .caches
                            .iter()
                            .any(|cache| cache.id == page.version.object.cache)
                        && self
                            .catalog
                            .borrow()
                            .current
                            .get(&object)
                            .is_some_and(|v| v.tag.as_bytes() == page.version.etag.as_bytes())
                })
                .collect();
            if entries.is_empty() {
                self.traffic(1, false);
                return;
            }
            let (node, worker, (page, entry)) = &entries[self.rng.pick(entries.len())];
            let object = usize::from_str_radix(&page.version.object.key.to_hex(), 16).unwrap();
            let mut client = self.request_on(object, true, false, *node);
            // The corruption probe must request the selected persisted page rather
            // than a random range that may entirely miss that record.
            client.first = page.number.0 as usize * PAGE_BYTES as usize;
            client.end = (client.first + PAGE_BYTES as usize).min(client.size);
            client.request = format!("POST /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{SUBSCRIPTION_FIELDS}If-Match: {}\r\nRange: bytes={}-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", key(object), client.tag, client.first, client.end - 1).into_bytes();
            let path = self.nodes[*node]
                .config
                .slab_directory
                .join(format!("worker-{worker}-slab-0.dat"));
            self.sim.disk().sync_all().unwrap();
            let offset = entry.location.extent.offset();
            let original = self
                .sim
                .disk()
                .read(&path, offset, 1, DiskState::Durable)
                .unwrap();
            self.sim
                .disk()
                .corrupt(&path, offset, &[original[0] ^ 0x80], DiskState::Both)
                .unwrap();
            for node in &self.nodes {
                for worker in &node.workers {
                    worker.app.memory.evict_idle(usize::MAX).unwrap();
                }
            }
            let reads = self
                .coverage
                .operations
                .get("complete:read")
                .copied()
                .unwrap_or(0);
            self.exchange(client, true);
            let read = self
                .coverage
                .operations
                .get("complete:read")
                .copied()
                .unwrap_or(0)
                > reads;
            self.sim
                .disk()
                .corrupt(&path, offset, &original, DiskState::Both)
                .unwrap();
            if read {
                self.coverage.action("disk-corruption");
            }
        }

        pub(super) fn cache_obligations(&mut self) {
            // Probe disk and memory hits on every run. The origin version is
            // unavailable, so refetching cannot disguise a cache failure. Actual
            // disk-path evidence contributes to default corpus coverage.
            let node = self.rng.pick(self.nodes.len());
            let client = self.request_on(2, false, false, node);
            self.exchange(client, false);
            let version = self.catalog.borrow_mut().current.remove(&2).unwrap();
            let calls = self.catalog.borrow().calls;
            let reads = self
                .coverage
                .operations
                .get("complete:read")
                .copied()
                .unwrap_or(0);
            for worker in self.nodes.iter().flat_map(|n| &n.workers) {
                worker.app.memory.evict_idle(usize::MAX).unwrap();
            }
            // Construct requests from independent version facts even with origin offline.
            self.catalog.borrow_mut().current.insert(2, version.clone());
            let client = self.request_on(2, true, false, node);
            self.catalog.borrow_mut().current.remove(&2);
            self.exchange(client, false);
            let disk_reads = self
                .coverage
                .operations
                .get("complete:read")
                .copied()
                .unwrap_or(0);
            assert_eq!(
                self.catalog.borrow().calls,
                calls,
                "disk hit reached origin"
            );
            if disk_reads > reads {
                self.coverage.action("verified-disk-hit");
            }
            self.catalog.borrow_mut().current.insert(2, version.clone());
            let client = self.request_on(2, true, false, node);
            self.catalog.borrow_mut().current.remove(&2);
            self.exchange(client, false);
            assert_eq!(
                self.coverage
                    .operations
                    .get("complete:read")
                    .copied()
                    .unwrap_or(0),
                disk_reads,
                "memory hit read disk"
            );
            assert_eq!(
                self.catalog.borrow().calls,
                calls,
                "memory hit reached origin"
            );
            self.coverage.action("verified-memory-hit");
            self.catalog.borrow_mut().current.insert(2, version);
        }

        pub(super) fn peer_outage(&mut self, env: &racer_io::Env) {
            let index = self.rng.pick(self.nodes.len());
            let address = self.nodes[index].config.peer_listen;
            let listener_scope = self.nodes[index].workers[0]
                .app
                .listener_scope
                .take()
                .unwrap();
            listener_scope.cancel().unwrap();
            self.nodes[index].workers[0].app.peer_task.take();
            let endpoint = crate::http::Endpoint::Peer(address.to_string());
            for node in &self.nodes {
                node.workers[0].app.http.invalidate(&endpoint);
            }
            // Fence the listener-owned accept and receive operations before probing.
            for _ in 0..8 {
                self.tick();
            }
            self.coverage.action("peer-outage");
            self.traffic(2, true);
            let node = &mut self.nodes[index];
            let peers = node.workers[0].app.peers.clone();
            let listener_scope =
                scope(&env.ctx.clock, Duration::from_secs(365 * 24 * 3600)).unwrap();
            let peer_scope = listener_scope.clone();
            node.workers[0].app.listener_scope = Some(listener_scope);
            node.workers[0].app.peer_task =
                Some(Box::pin(
                    async move { peers.listen(address, &peer_scope).await },
                ));
            self.tick();
            self.coverage.action("peer-heal");
            self.traffic(1, false);
        }

        pub(super) fn partition_traffic(&mut self) {
            // Partition a generated cut after normal traffic has established pooled
            // streams. No endpoint invalidation or membership rewrite accompanies it.
            self.traffic(2, false);
            let mut clients: Vec<_> = (0..4)
                .map(|_| {
                    let object = self.rng.pick(8);
                    self.request(object, false, false)
                })
                .collect();
            for _ in 0..16 {
                self.tick();
                for client in &mut clients {
                    if !client.done && client.poll() {
                        self.check(client, false);
                        client.done = true;
                        client.fd.take();
                    }
                }
            }
            let cut = 1 + self.rng.pick(self.nodes.len() - 1);
            let mut links = Vec::new();
            for a in &self.nodes[..cut] {
                for b in &self.nodes[cut..] {
                    let pair = (
                        SocketAddress::Inet(a.config.peer_listen),
                        SocketAddress::Inet(b.config.peer_listen),
                    );
                    self.sim.partition(pair.0.clone(), pair.1.clone());
                    links.push(pair);
                }
            }
            for _ in 0..100 + self.rng.pick(100) {
                self.tick();
                for client in &mut clients {
                    if !client.done && client.poll() {
                        self.check(client, true);
                        client.done = true;
                        client.fd.take();
                    }
                }
            }
            for (a, b) in links {
                self.sim.heal(a, b);
            }
            for client in clients {
                if !client.done {
                    self.exchange(client, true);
                }
            }
            self.settle();
            self.coverage.action("partition-heal");
        }

        pub(super) fn crash_inflight(&mut self, env: &racer_io::Env) {
            let node = self.rng.pick(self.nodes.len());
            self.update(1);
            let mut client = self.request_on(1, false, false, node);
            let mut admitted = false;
            for _ in 0..MAX_TURNS {
                self.tick();
                if client.poll() {
                    break;
                }
                if self.nodes[node].workers.iter().any(|w| {
                    w.app.drivers.pending() != 0
                        || w.runtime.crypto.outstanding() != 0
                        || w.app.store.writer.slabs().writes_in_flight() != 0
                }) {
                    admitted = true;
                    break;
                }
            }
            assert!(admitted, "crash must interrupt accepted work");
            let id = self.retire(env, node, true);
            self.exchange(client, true);
            self.add(env, Some(id));
            self.coverage.action("inflight-crash");
        }

        pub(super) fn crash_pending_write(&mut self, env: &racer_io::Env) {
            self.update(7);
            self.sim.inject("writev", Fault::Delay(64)).unwrap();
            *self.coverage.injected.entry("writev".into()).or_default() += 1;
            let mut client = self.request(7, false, false);
            let mut completed = false;
            let mut victim = None;
            for _ in 0..MAX_TURNS {
                self.tick();
                if !completed && client.poll() {
                    self.check(&client, false);
                    completed = true;
                    client.fd.take();
                }
                victim = self.nodes.iter().position(|n| {
                    n.workers
                        .iter()
                        .any(|w| w.app.store.writer.slabs().writes_in_flight() != 0)
                });
                if victim.is_some() {
                    break;
                }
            }
            let victim = victim.expect("generated write never reached OS submission");
            let id = self.retire(env, victim, true);
            if !completed {
                self.exchange(client, true);
            }
            self.add(env, Some(id));
            self.coverage.action("pending-write-crash");
        }

        pub(super) fn key_retirement(&mut self) {
            self.key_epoch = self.key_epoch.checked_add(1).expect("key epoch exhausted");
            assert!(self.key_epoch < 100);
            self.generation += 1;
            let bundles: Vec<_> = self
                .nodes
                .iter()
                .map(|n| self.bundle(&n.config, self.generation + 1000))
                .collect();
            for (node, bundle) in self.nodes.iter_mut().zip(bundles) {
                node.workers[0].app.keys.install(bundle).unwrap();
                node.workers[0].app.control = node.control.clone();
            }
            // Key admission changes synchronously; existing native/crypto owners
            // continue through their own completion protocol during normal ticks.
            self.tick();
            for node in &mut self.nodes {
                node.workers[0].app.control = None;
                node.workers[0].app.control_task.take();
            }
            self.coverage.action("key-retirement");
        }

        pub(super) fn cache_recreate(&mut self, env: &racer_io::Env) {
            self.generation += 1;
            let members = self.members();
            let mut staged = Vec::new();
            for node in &mut self.nodes {
                node.workers[0].app.control = node.control.clone();
                staged.push(CachePublication {
                    proposal: RefCell::new(None),
                    local: None,
                    served: Rc::new(RefCell::new(None)),
                    node: node.workers[0].app.node.clone(),
                    listeners: node.workers[0].app.prepared_listeners.clone(),
                    capacity: node.config.limits.metadata_entries.get(),
                });
            }
            let mut committed = vec![false; self.nodes.len()];
            for _ in 0..MAX_TURNS {
                for (index, adapter) in staged.iter().enumerate() {
                    if committed[index] {
                        continue;
                    }
                    match adapter.stage(&[]) {
                        Ok(transition) => {
                            let node = &self.nodes[index];
                            let mut publication =
                                publication(&node.config, self.generation, vec![]);
                            publication.membership_version = MembershipVersion(self.generation);
                            publication.members = members.iter().cloned().map(Into::into).collect();
                            node.workers[0]
                                .app
                                .snapshots
                                .apply_staged(publication, Some(transition))
                                .unwrap();
                            committed[index] = true;
                        }
                        Err(Error::Unavailable) => (),
                        Err(error) => panic!("cache stage: {error:?}"),
                    }
                }
                self.tick();
                if committed.iter().all(|v| *v) {
                    break;
                }
            }
            assert!(committed.iter().all(|v| *v));
            self.cache_epoch += 1;
            self.key_retirement();
            self.publish();
            for node in &mut self.nodes {
                let definitions = node.workers[0]
                    .app
                    .snapshots
                    .current()
                    .unwrap()
                    .caches
                    .clone();
                let LocalWorker {
                    app,
                    runtime,
                    crypto,
                } = &mut node.workers[0];
                drive_local(
                    runtime,
                    &mut **crypto,
                    app.clients.reconcile(
                        &definitions,
                        &scope(&env.ctx.clock, Duration::from_secs(30)).unwrap(),
                    ),
                )
                .unwrap();
            }
            self.coverage.action("cache-retire-recreate");
        }

        pub(super) fn native_fault(&mut self, env: &racer_io::Env) {
            use rdma_verbs::simulation::Fault as NativeFault;
            use rdma_verbs::simulation::Operation as NativeOperation;
            if self.native_rules.is_empty() {
                for op in [
                    NativeOperation::Bind,
                    NativeOperation::WriteImm,
                    NativeOperation::Invalidate,
                ] {
                    for fault in [
                        NativeFault::Reject,
                        NativeFault::Delay(3 + self.rng.pick(8)),
                        NativeFault::Completion(1),
                    ] {
                        self.native_rules.push((op, fault));
                    }
                }
            }
            let index = self.rng.pick(self.native_rules.len());
            let (operation, fault) = self.native_rules.swap_remove(index);
            // Even a one-node generated topology needs a real peer payload path.
            if self.nodes.len() == 1 {
                self.add(env, None);
            }
            self.settle();
            self.clock.advance(Duration::from_secs(26));
            let object = self.object_id(5);
            let candidates = self.ranked_nodes(&object);
            let source = candidates[0];
            // The fault is consumed only if the read crosses the link set up
            // below. In larger topologies not every pair shares an overlay
            // edge, and a non-neighbor routes through a relay instead, so pick
            // the best-ranked candidate that is a direct neighbor of `source`.
            let membership = self.nodes[0].workers[0]
                .app
                .snapshots
                .current()
                .unwrap()
                .membership
                .clone();
            let source_id = self.nodes[source].config.node.clone();
            let receiver = candidates[1..]
                .iter()
                .copied()
                .find(|&n| membership.is_neighbor(&source_id, &self.nodes[n].config.node))
                .unwrap_or(candidates[1]);
            self.update(5);
            // Candidates probe predecessors CopyOnly before filling from origin.
            // Warm the primary's exact version first, including in two-node clusters.
            // The byte oracle remains independent of this injection-path selection.
            let warm = self.request_on(5, true, false, source);
            self.exchange(warm, false);
            // Pages use HTTP until the persistent link is ready.
            self.connect_links(env, receiver, source);
            self.fabric.fault(operation, fault);
            self.coverage.action("native-fault");
            let client = self.request_on(5, true, false, receiver);
            self.exchange(client, true);
            assert_eq!(self.fabric.pending_faults(), 0, "native fault not consumed");
            *self
                .coverage
                .native_faults
                .entry(format!(
                    "{operation:?}:{}",
                    match fault {
                        NativeFault::Reject => "reject",
                        NativeFault::Delay(_) => "delay",
                        NativeFault::Completion(_) => "completion",
                        NativeFault::Drop => "drop",
                        NativeFault::Reorder => "reorder",
                        NativeFault::DuplicateImmediate => "duplicate-immediate",
                        NativeFault::ByteLength(_) => "byte-length",
                        NativeFault::AddressError => "address-error",
                        NativeFault::RouteError => "route-error",
                        NativeFault::Timeout => "timeout",
                    }
                ))
                .or_default() += 1;
            self.coverage.action("native-fault-observed");
        }
    }
}
mod scenarios {
    //! Targeted regressions using the same assembled multi-node harness as the corpus.
    use super::*;

    pub(super) fn worker_subscriptions_contend_across_servers_and_recover_after_release() {
        use crate::peer::protocol::FetchMode;
        use crate::peer::protocol::Operation as PeerOperation;
        use crate::peer::protocol::PeerRequest;
        use crate::peer::protocol::PeerResponse;
        use crate::peer::subscriptions::Subscription;
        use crate::security::OriginContext;
        use crate::topology::RouteBudget;
        use flow_control::subscriptions::Interval;
        use racer_core::subscriptions::Demand;

        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(73);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(73, sim, clock, false);
        harness.add(&env, None);
        harness.add(&env, None);
        let target = harness
            .nodes
            .iter()
            .position(|n| n.workers.len() == 2)
            .unwrap();
        let source = 1 - target;
        let receiver = &harness.nodes[target];
        let servers = receiver
            .workers
            .iter()
            .map(|w| w.app.peers.clone())
            .collect::<Vec<_>>();
        let membership = receiver.workers[0]
            .app
            .snapshots
            .current()
            .unwrap()
            .membership
            .clone();
        let destination = receiver.config.node.clone();
        let sender = &harness.nodes[source];
        let keys = sender.workers[0].app.keys.clone();
        let forwarding = Forwarding::new(Rc::new(Signatures::new(
            keys.clone(),
            Rc::new(Certificates::new(
                sender.config.cluster.clone(),
                keys.clone(),
            )),
            env.ctx.clone(),
        )));
        let credentials = CredentialCrypto::new(
            keys,
            sender.workers[0].runtime.admission.clone(),
            env.ctx.clone().rng,
        );
        let scope = scope(&env.ctx.clock, Duration::from_secs(5)).unwrap();
        let object = ObjectId {
            cache: harness.definition(0).id,
            key: CacheKey([0; 32]),
        };
        let signed = |sequence: u8| {
            let attempt = AttemptId([sequence + 1; 16]);
            let request = PeerRequest {
                operation: PeerOperation::Subscribe {
                    subscription: Subscription {
                        id: [42; 16],
                        sequence: sequence.into(),
                        version: ObjectVersion {
                            object: object.clone(),
                            etag: StrongEtag::test_value("absent"),
                        },
                        demand: Demand::new(vec![Interval { start: 0, end: 1 }]).unwrap(),
                        page_budget: 3 - u32::from(sequence),
                        byte_budget: PAGE_BYTES + 16,
                        flights: None,
                    },
                    mode: FetchMode::CopyOnly,
                },
                origin: credentials
                    .seal(
                        &OriginContext {
                            object: object.clone(),
                            metadata: None,
                            authorization: None,
                        },
                        attempt,
                        &scope,
                    )
                    .unwrap(),
                route: RouteBudget {
                    membership: membership.version,
                    request: scope.request,
                    attempt,
                    destination: destination.clone(),
                    visited: vec![sender.config.node.clone()],
                    remaining_links: 4,
                    remaining_attempts: 0,
                    deadline: scope.deadline,
                },
            };
            forwarding.sign_request_to(request, &destination).unwrap()
        };
        let (first, _) = signed(0);
        let (contending, binding) = signed(1);
        let (retry, retry_binding) = signed(2);
        let waker = wake::waker();
        let mut cx = Context::from_waker(&waker);
        // Drive only verification, not the owner mailbox. The first server must
        // hold a live selection before the other server finishes verification.
        let mut held = servers[0].dispatch(first, &scope);
        assert!(held.as_mut().poll(&mut cx).is_pending());
        let worker = &mut harness.nodes[target].workers[0];
        worker.crypto.poll_budgeted(&mut cx, 1).unwrap();
        worker.runtime.crypto.poll_budgeted(1).unwrap();
        assert!(held.as_mut().poll(&mut cx).is_pending());
        let mut rejected = servers[1].dispatch(contending, &scope);
        assert!(rejected.as_mut().poll(&mut cx).is_pending());
        let worker = &mut harness.nodes[target].workers[1];
        worker.crypto.poll_budgeted(&mut cx, 1).unwrap();
        worker.runtime.crypto.poll_budgeted(1).unwrap();
        let Poll::Ready(Ok(response)) = rejected.as_mut().poll(&mut cx) else {
            panic!("second server must reject the same live subscription");
        };
        assert!(matches!(
            forwarding
                .verify_response(response, &binding)
                .unwrap()
                .response(),
            PeerResponse::Overloaded
        ));
        drop(rejected);
        drop(held);

        let mut recovered = servers[1].dispatch(retry, &scope);
        assert!(
            recovered.as_mut().poll(&mut cx).is_pending(),
            "released selection admits the other worker"
        );
        let response = (0..100)
            .find_map(|_| {
                harness.tick();
                match recovered.as_mut().poll(&mut cx) {
                    Poll::Ready(result) => Some(result.unwrap()),
                    Poll::Pending => None,
                }
            })
            .expect("accepted copy-only request completes through its owner");
        assert!(matches!(
            forwarding
                .verify_response(response, &retry_binding)
                .unwrap()
                .response(),
            PeerResponse::Miss
        ));
    }

    pub(super) fn completed_peer_dispatches_do_not_exhaust_worker_cancellation() {
        use crate::model::MetadataSelector;
        use crate::peer::protocol::FetchMode;
        use crate::peer::protocol::Operation as PeerOperation;
        use crate::peer::protocol::PeerRequest;
        use crate::peer::protocol::PeerResponse;
        use crate::security::OriginContext;
        use crate::topology::RouteBudget;
        use futures::Stream;
        use futures::stream::FuturesUnordered;

        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(73);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(73, sim, clock, false);
        harness.add(&env, None);
        harness.add(&env, None);
        let target = harness
            .nodes
            .iter()
            .position(|n| n.workers.len() == 2)
            .unwrap();
        let source = 1 - target;
        let app = &harness.nodes[target].workers[0].app;
        let mut object = ObjectId {
            cache: harness.definition(0).id,
            key: CacheKey([0; 32]),
        };
        while app.directory.metadata_owner(&object).unwrap() != WorkerId(1) {
            object.key.0[0] += 1;
        }
        let server = app.peers.clone();
        // Exactly the long-lived scope used by WorkerApplication's ingress loop.
        let worker_scope = app.task_scope.clone().unwrap();
        let membership = app.snapshots.current().unwrap().membership.clone();
        let destination = harness.nodes[target].config.node.clone();
        let sender = &harness.nodes[source].workers[0].app;
        let signatures = Rc::new(Signatures::new(
            sender.keys.clone(),
            Rc::new(Certificates::new(
                harness.nodes[source].config.cluster.clone(),
                sender.keys.clone(),
            )),
            env.ctx.clone(),
        ));
        let forwarding = Forwarding::new(signatures);
        let credentials = CredentialCrypto::new(
            sender.keys.clone(),
            sender.runtime.admission.clone(),
            env.ctx.clone().rng,
        );
        let previous = harness.nodes[source].config.node.clone();
        for round in 0u64..1100 {
            let scope = RequestScope::on(
                &env.ctx.clock,
                RequestId([1; 16]),
                env.ctx.now() + Duration::from_secs(5),
            )
            .unwrap();
            let mut attempt = [0; 16];
            attempt[..8].copy_from_slice(&round.to_le_bytes());
            let attempt = AttemptId(attempt);
            let context = OriginContext {
                object: object.clone(),
                metadata: None,
                authorization: None,
            };
            let request = PeerRequest {
                operation: PeerOperation::Metadata {
                    object: object.clone(),
                    selector: MetadataSelector::Fresh,
                    mode: FetchMode::CopyOnly,
                },
                origin: credentials.seal(&context, attempt, &scope).unwrap(),
                route: RouteBudget {
                    membership: membership.version,
                    request: scope.request,
                    attempt,
                    destination: destination.clone(),
                    visited: vec![previous.clone()],
                    remaining_links: 4,
                    remaining_attempts: 0,
                    deadline: scope.deadline,
                },
            };
            let (request, binding) = forwarding.sign_request_to(request, &destination).unwrap();
            let mut effective = worker_scope.clone();
            effective.request = scope.request;
            effective.deadline = scope.deadline;
            // FuturesUnordered gives each completed ingress its own task waker, just
            // like the production ingress collection. No synthetic registrations.
            let server = server.clone();
            let mut pending = FuturesUnordered::new();
            pending.push(Box::pin(async move {
                server.dispatch(request, &effective).await
            }));
            let mut response = None;
            for _ in 0..100 {
                {
                    let app = &harness.nodes[target].workers[0].app;
                    let _local = app
                        .directory
                        .simulation_scope(Some((app.worker, app.coordinator.clone())));
                    if let Poll::Ready(Some(result)) = std::pin::Pin::new(&mut pending)
                        .poll_next(&mut Context::from_waker(&wake::waker()))
                    {
                        response = Some(result.unwrap());
                        break;
                    }
                }
                harness.tick();
            }
            let verified = forwarding
                .verify_response(response.expect("bounded peer dispatch"), &binding)
                .unwrap();
            assert!(
                matches!(verified.response(), PeerResponse::Miss),
                "round {round}: expected Miss; overloaded={}",
                matches!(verified.response(), PeerResponse::Overloaded)
            );
            for worker in &harness.nodes[target].workers {
                assert_eq!(worker.runtime.admission.used(ResourceClass::Waiter), 0);
                assert_eq!(worker.runtime.admission.used(ResourceClass::Flight), 0);
                assert_eq!(worker.app.drivers.pending(), 0);
            }
        }
        assert!(worker_scope.check().is_ok());
    }

    pub(super) fn healthy_relayed_page_reads() {
        healthy_relayed_page_reads_with_mode(false);
    }

    pub(super) fn listeners_survive_repeated_reactor_queue_pressure() {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(73);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(73, sim.clone(), clock.clone(), false);
        harness.add(&env, None);
        // The DST harness bypasses control enrollment and already attaches diagnostic
        // resources; install the same service task as start_diagnostics.
        let app = &mut harness.nodes[0].workers[0].app;
        let diagnostic_scope = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
        app.diagnostic_scope = Some(diagnostic_scope.clone());
        let telemetry = app.telemetry.clone();
        let listen = app.diagnostics_address;
        app.diagnostic_task = Some(Box::pin(async move {
            telemetry.serve(listen, &diagnostic_scope).await
        }));
        harness.tick();
        let reactor = harness.nodes[0].workers[0].runtime.reactor.clone();
        let limit = harness.nodes[0].config.limits.queue_entries.get();
        let address = SocketAddress::Inet(harness.nodes[0].config.diagnostics_listen);
        let waker = wake::waker();
        let mut cx = Context::from_waker(&waker);
        for round in 0..4 {
            // Complete the existing diagnostic accept, then fill the ordinary
            // partition before the service consumes that CQE and submits its receive.
            let probe = sim.connect(address.clone()).unwrap();
            let peer = sim
                .connect(SocketAddress::Inet(harness.nodes[0].config.peer_listen))
                .unwrap();
            reactor.poll_budgeted(128).unwrap();
            reactor.poll_budgeted(128).unwrap();
            let pressure_scope = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
            let (reader, _writer) = sim.socket_pair();
            let reader = Rc::new(reader);
            let mut pressure = Vec::new();
            loop {
                let mut wait =
                    reactor.readiness(reader.clone(), libc::POLLIN as u32, &pressure_scope);
                match wait.as_mut().poll(&mut cx) {
                    Poll::Pending => pressure.push(wait),
                    Poll::Ready(Err(Error::Overloaded)) => break,
                    _ => panic!("unexpected pressure result"),
                }
            }
            assert!(!pressure.is_empty());
            let ordinary_in_flight = reactor.in_flight();
            assert!(ordinary_in_flight < limit);
            for _ in 0..32 {
                harness.nodes[0].workers[0]
                    .app
                    .poll_budgeted(&mut cx, 64)
                    .unwrap();
                assert!(reactor.in_flight() >= ordinary_in_flight);
                assert!(reactor.in_flight() <= limit);
                assert!(harness.nodes[0].workers[0].app.diagnostic_task.is_some());
                assert!(harness.nodes[0].workers[0].app.peer_task.is_some());
            }
            drop(probe);
            drop(peer);
            pressure_scope.cancel().unwrap();
            drop(pressure);
            for _ in 0..32 {
                harness.tick();
            }
            for path in ["/readyz", "/metrics"] {
                let socket = sim.connect(address.clone()).unwrap();
                let request = format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n");
                assert_eq!(
                    handle(&socket).send(request.as_bytes()).unwrap(),
                    request.len()
                );
                let mut response = Vec::new();
                let mut closed = false;
                for _ in 0..1000 {
                    harness.tick();
                    let mut bytes = [0; 4096];
                    match handle(&socket).recv(&mut bytes) {
                        Ok(0) => {
                            closed = true;
                            break;
                        }
                        Ok(count) => response.extend_from_slice(&bytes[..count]),
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                        Err(error) => panic!("{error}"),
                    }
                }
                assert!(closed, "round {round} {path} stalled");
                assert!(response.starts_with(b"HTTP/1.1 200 OK\r\n"), "{response:?}");
            }
            assert!(reactor.in_flight() < limit);
        }
        let worker = &mut harness.nodes[0].workers[0];
        worker.app.stop_admission().unwrap();
        worker
            .app
            .diagnostic_scope
            .as_ref()
            .unwrap()
            .cancel()
            .unwrap();
        for _ in 0..32 {
            worker.runtime.reactor.poll_budgeted(128).unwrap();
            worker.app.poll_budgeted(&mut cx, 64).unwrap();
        }
        assert!(worker.app.diagnostic_task.is_none());
        assert!(worker.app.peer_task.is_none());
        assert_eq!(reactor.in_flight(), 0);
    }

    pub(super) fn healthy_relayed_page_reads_opaque_opt_in() {
        healthy_relayed_page_reads_with_mode(true);
    }

    /// Cluster size for relayed read scenarios: above the 64 member full mesh
    /// of mesh v2, so some reads need a relay.
    const RELAY_NODES: usize = 65;

    /// Nodes that read in the healthy relayed scenario.
    const READERS: usize = 40;

    fn healthy_relayed_page_reads_with_mode(opaque: bool) {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(71);
        let env = &racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(71, sim, clock, false);
        harness.opaque_relay = opaque;
        // Match the deployed 64 MiB per-worker payload budgets. A five-page layer
        // exceeds resident capacity and forces receive admission to reclaim idle data.
        harness.payload_regression = true;
        harness.update(1);
        for _ in 0..RELAY_NODES {
            harness.add(env, None);
        }
        for node in &harness.nodes {
            for worker in &node.workers {
                assert_eq!(worker.app.peers.opaque_relay(), opaque);
            }
        }
        for node in 0..READERS {
            let mut client = harness.request_on(1, true, false, node);
            client.first = 0;
            client.end = client.size;
            // Match client credit to this small payload budget. Delivered leases no
            // longer occupy the independent server acquisition window.
            client.request = format!("POST /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: 2\r\nRacer-Byte-Credits: {}\r\nIf-Match: {}\r\nRange: bytes=0-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", key(1), 2 * PAGE_BYTES, client.tag, client.size - 1).into_bytes();
            harness.exchange(client, false);
        }
        assert_eq!(harness.coverage.success, READERS);
        assert_eq!(harness.coverage.failures, 0);
        assert_eq!(
            harness.coverage.bytes,
            READERS * (4 * PAGE_BYTES as usize + 257)
        );
        assert!(harness.coverage.relay_turns > 0);
    }

    pub(super) fn concurrent_relayed_layers_diagnose_receive_pressure_and_recover() {
        concurrent_relayed_layers_with_mode(false);
    }

    pub(super) fn concurrent_relayed_layers_opaque_opt_in_pressure_and_recovery() {
        concurrent_relayed_layers_with_mode(true);
    }

    fn concurrent_relayed_layers_with_mode(opaque: bool) {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(73);
        let env = &racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(73, sim, clock, false);
        harness.concurrent_layers = true;
        harness.opaque_relay = opaque;
        for object in 1..=4 {
            harness.update(object);
        }
        for _ in 0..RELAY_NODES {
            harness.add(env, None);
        }
        // Exercise success, then controlled receive pressure after success headers,
        // then recovery on the same graphs, identities, disk, and connections.
        for pressure in [false, true, false] {
            let mut clients: Vec<_> = (1..=4).map(|object| {
                let mut client = harness.request_on(object, true, false, 0);
                client.first = 0;
                client.end = client.size;
                client.request = format!("POST /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{SUBSCRIPTION_FIELDS}If-Match: {}\r\nRange: bytes=0-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", key(object), client.tag, client.size - 1).into_bytes();
                client
            }).collect();
            let mut charges = Vec::new();
            let mut injected = false;
            let before = harness.coverage.failures;
            for _ in 0..MAX_TURNS {
                harness.tick();
                for client in &mut clients {
                    if !client.done && client.poll() {
                        harness.check(client, pressure);
                        client.fd.take();
                        client.done = true;
                    }
                }
                if pressure
                    && !injected
                    && clients
                        .iter()
                        .any(|client| client.response.len() > 32768 && !client.done)
                {
                    for node in &harness.nodes {
                        for worker in &node.workers {
                            worker.app.memory.evict_idle(usize::MAX).unwrap();
                            let admission = &worker.runtime.admission;
                            let free = admission.limit(ResourceClass::Ciphertext)
                                - admission.used(ResourceClass::Ciphertext);
                            if free != 0 {
                                charges.push(
                                    admission
                                        .reserve(None, ResourceClass::Ciphertext, free)
                                        .unwrap(),
                                );
                            }
                        }
                    }
                    injected = true;
                }
                if clients.iter().all(|client| client.done) {
                    break;
                }
            }
            assert!(
                clients.iter().all(|client| client.done),
                "concurrent layer reads stalled"
            );
            if pressure {
                assert!(injected);
                assert!(harness.coverage.failures > before);
                let mut diagnostics = String::new();
                for node in &harness.nodes {
                    node.workers[0]
                        .app
                        .telemetry
                        .failures
                        .write(&mut diagnostics)
                        .unwrap();
                }
                assert!(
                    diagnostics.contains("stage=Admission error=Overloaded"),
                    "{diagnostics}"
                );
                assert!(diagnostics.contains("stage=NextSlice"), "{diagnostics}");
                assert!(diagnostics.contains("class: Ciphertext"), "{diagnostics}");
                assert!(
                    diagnostics.contains("stage=CandidateResponse")
                        || diagnostics.contains("stage=PeerReceiveAdmission"),
                    "{diagnostics}"
                );
            } else {
                assert_eq!(harness.coverage.failures, before);
            }
            drop(charges);
            harness.settle();
        }
        assert!(harness.coverage.secondary_worker_turns > 0);
        assert!(harness.coverage.relay_turns > 0);
        // Exact deterministic count under injected pressure. Delivery mode,
        // replica selection, receive charge sizes, and memory probes change turn
        // ordering, so this pins the default `spread` selection with aligned TCP
        // receive, which charges each body's whole pooled size class.
        assert_eq!(harness.coverage.success, 8, "opaque={opaque}");
    }
}
mod traffic {
    use super::*;
    impl ReplayTrace {
        pub(super) fn record(&mut self, event: String) {
            self.hash.update((event.len() as u64).to_le_bytes());
            self.hash.update(event.as_bytes());
            self.events += 1;
            if self.recent.len() == 32 {
                self.recent.pop_front();
            }
            self.recent.push_back(event);
        }
        pub(super) fn digest(&self) -> [u8; 32] {
            self.hash.clone().finalize().into()
        }
        pub(super) fn checkpoint(&mut self) {
            assert!(self.checkpoints.len() < 514, "bounded replay checkpoints");
            self.checkpoints.push((self.events, self.digest()));
        }
    }
    impl Coverage {
        pub(super) fn add_to_corpus(&self, counts: &mut BTreeMap<String, usize>, native: bool) {
            for (name, count) in self.corpus_counts(native) {
                *counts.entry(name).or_default() += count;
            }
        }
        // Aggregate path evidence only after per-run correctness and replay checks.
        pub(super) fn corpus_counts(&self, native: bool) -> BTreeMap<String, usize> {
            let mut counts = BTreeMap::new();
            for (name, count) in [
                ("successful-responses", self.success),
                ("response-bytes", self.bytes),
                ("origin-gets", self.origin_gets),
                ("persisted-records", self.persisted),
                ("secondary-worker-data-turns", self.secondary_worker_turns),
                ("relay-turns", self.relay_turns),
            ] {
                counts.insert(name.into(), count);
            }
            for (prefix, names, observed) in [
                (
                    "action",
                    &[
                        "multi-worker",
                        "key-retirement",
                        "cache-retire-recreate",
                        "inflight-crash",
                        "pending-write-crash",
                        "partition-heal",
                        "wall-jump",
                        "malformed-client",
                        "peer-replay",
                        "peer-signature-corruption",
                        "disk-corruption",
                        "verified-disk-hit",
                        "verified-memory-hit",
                    ][..],
                    &self.actions,
                ),
                (
                    "origin",
                    &[
                        "credential-reject",
                        "credential-forbidden",
                        "origin-duplicate-length",
                        "origin-truncated-body",
                        "origin-malformed-etag",
                    ][..],
                    &self.origin_faults,
                ),
            ] {
                for name in names {
                    counts.insert(
                        format!("{prefix}:{name}"),
                        observed.get(name).copied().unwrap_or(0),
                    );
                }
            }
            for operation in ["complete:write", "complete:accept", "blocked:send"] {
                counts.insert(
                    format!("os:{operation}"),
                    self.operations.get(operation).copied().unwrap_or(0),
                );
            }
            if native {
                counts.insert("native-writes".into(), self.native_writes);
                for operation in ["Bind", "WriteImm", "Invalidate"] {
                    for fault in ["reject", "delay", "completion"] {
                        let rule = format!("{operation}:{fault}");
                        counts.insert(
                            format!("native:{rule}"),
                            self.native_faults.get(&rule).copied().unwrap_or(0),
                        );
                    }
                }
            }
            counts
        }
        pub(super) fn collect_native(&mut self, fabric: &rdma_verbs::simulation::Simulation) {
            for event in fabric.take_trace() {
                self.native_writes += usize::from(
                    event.operation == rdma_verbs::simulation::Operation::WriteImm
                        && event.completion
                        && event.result == 0,
                );
                self.trace.record(format!(
                    "native:{:?}:{}:{:?}:{}:{}",
                    event.operation, event.resource, event.work_id, event.result, event.completion
                ));
            }
        }
        pub(super) fn action(&mut self, name: &'static str) {
            self.trace.record(format!("action:{name}"));
            *self.actions.entry(name).or_default() += 1;
        }
        pub(super) fn collect(&mut self, sim: &SimOs) {
            for event in sim.take_trace() {
                self.trace.record(format!(
                    "os:{}:{}:{}",
                    event.operation, event.resource, event.result
                ));
                if let Some(op) = event.operation.strip_prefix("fault:") {
                    *self.observed.entry(op.into()).or_default() += 1;
                }
                if (event.operation.starts_with("complete:") && event.result >= 0)
                    || event.operation.starts_with("blocked:")
                    || event.operation == "disk:crash"
                {
                    *self.operations.entry(event.operation).or_default() += 1;
                }
            }
        }
    }
    impl Client {
        pub(super) fn poll(&mut self) -> bool {
            let fd = handle(self.fd.as_ref().unwrap());
            if self.released < self.releases.len() {
                match fd.send(&self.releases[self.released..]) {
                    Ok(n) => self.released += n,
                    Err(e) if would_block(&e) => (),
                    Err(_) => {
                        self.disconnected = true;
                        return true;
                    }
                }
            }
            if self.sent < self.request.len() {
                match fd.send(&self.request[self.sent..]) {
                    Ok(n) => self.sent += n,
                    Err(e) if would_block(&e) => return false,
                    Err(_) => {
                        self.disconnected = true;
                        return true;
                    }
                }
            }
            let mut bytes = [0; 65536];
            match fd.recv(&mut bytes) {
                Ok(0) => {
                    self.disconnected = true;
                    return true;
                }
                Ok(n) => self.response.extend_from_slice(&bytes[..n]),
                Err(e) if would_block(&e) => return false,
                Err(_) => {
                    self.disconnected = true;
                    return true;
                }
            }
            assert!(
                self.response.len() <= self.size.max(2 * PAGE_BYTES as usize) + 32768,
                "unbounded client response"
            );
            if self.response.windows(4).any(|w| w == b"\r\n\r\n") {
                let (_, h, end) = headers(&self.response);
                let length: usize = h["content-length"].parse().unwrap();
                if !self.head && h.contains_key("racer-range-start") {
                    let mut cursor = self.frame_cursor.unwrap_or(end);
                    while cursor + 21 <= self.response.len() {
                        let count = u32::from_be_bytes(
                            self.response[cursor + 17..cursor + 21].try_into().unwrap(),
                        ) as usize;
                        if cursor + 21 + count > self.response.len() {
                            break;
                        }
                        if self.response[cursor] == 1 {
                            self.releases
                                .extend_from_slice(&self.response[cursor + 1..cursor + 9]);
                            self.releases
                                .extend_from_slice(&(count as u32).to_be_bytes());
                        }
                        cursor += 21 + count;
                    }
                    self.frame_cursor = Some(cursor);
                }
                self.response.len() >= end + if self.head { 0 } else { length }
            } else {
                false
            }
        }
    }
    impl Harness {
        pub(super) fn generated(&mut self, env: &racer_io::Env, steps: usize) {
            for object in 0..8 {
                self.update(object);
            }
            let initial = 2 + self.rng.pick(MAX_NODES - 1);
            for _ in 0..initial {
                self.add(env, None);
            }
            let mut actions = Vec::new();
            for step in 0..steps {
                if actions.is_empty() {
                    actions.extend_from_slice(WEIGHTED_ACTIONS);
                }
                let selected = self.rng.pick(actions.len());
                let action = actions.swap_remove(selected);
                self.coverage.trace.record(format!(
                    "step:{step}:{action:?}:{}:{}",
                    self.nodes.len(),
                    self.rng.0
                ));
                eprintln!(
                    "dst seed={} step={step} action={action:?} nodes={}",
                    self.seed,
                    self.nodes.len()
                );
                self.run_action(env, action);
                self.coverage.trace.checkpoint();
            }
            self.verify_recovery();
            self.teardown_generated(env);
        }
        fn run_action(&mut self, env: &racer_io::Env, action: Action) {
            match action {
                Action::AddNode if self.nodes.len() < MAX_NODES => self.add(env, None),
                Action::RemoveNode if self.nodes.len() > 1 => {
                    let index = self.rng.pick(self.nodes.len());
                    self.remove(env, index);
                }
                Action::Update => {
                    let object = self.rng.pick(8);
                    self.update(object);
                }
                Action::Evict => {
                    self.settle();
                    for worker in self.nodes.iter().flat_map(|n| &n.workers) {
                        worker.app.memory.evict_idle(usize::MAX).unwrap();
                    }
                    self.coverage.action("evict");
                    self.traffic(1, false);
                }
                Action::Restart => {
                    let index = self.rng.pick(self.nodes.len());
                    let id = self.remove(env, index);
                    self.add(env, Some(id));
                }
                Action::ShortIo => {
                    let operation = if self.rng.pick(2) == 0 {
                        "send"
                    } else {
                        "recv"
                    };
                    self.sim
                        .inject(operation, Fault::Short(1 + self.rng.pick(128)))
                        .unwrap();
                    *self.coverage.injected.entry(operation.into()).or_default() += 1;
                    self.coverage.action("short-io");
                    self.traffic(1, false);
                }
                Action::ConnectFailure => {
                    self.sim
                        .inject("connect", Fault::Errno(libc::ECONNREFUSED))
                        .unwrap();
                    *self.coverage.injected.entry("connect".into()).or_default() += 1;
                    // A new version forces an origin connection despite warm peers.
                    self.update(2);
                    let client = self.request(2, false, false);
                    self.exchange(client, true);
                    self.coverage.action("connect-failure");
                    self.recover_origin();
                }
                Action::DelayedWrite => {
                    self.sim
                        .inject("writev", Fault::Delay(3 + self.rng.pick(8)))
                        .unwrap();
                    *self.coverage.injected.entry("writev".into()).or_default() += 1;
                    self.update(3);
                    let client = self.request(3, false, false);
                    self.exchange(client, false);
                    self.coverage.action("delayed-write");
                }
                Action::ClientCancel => {
                    let mut client = self.request(1, false, false);
                    // Submit without draining the response to force socket backpressure.
                    for _ in 0..MAX_TURNS {
                        if client.sent == client.request.len() {
                            break;
                        }
                        if let Ok(n) =
                            handle(client.fd.as_ref().unwrap()).send(&client.request[client.sent..])
                        {
                            client.sent += n;
                        }
                        self.tick();
                    }
                    for _ in 0..256 {
                        self.tick();
                    }
                    for _ in 0..1 + self.rng.pick(32) {
                        self.tick();
                        if client.poll() {
                            break;
                        }
                    }
                    self.sim.disconnect(client.fd.as_ref().unwrap()).unwrap();
                    client.fd.take();
                    self.settle();
                    self.coverage.action("client-cancel");
                }
                Action::PeerOutage => self.peer_outage(env),
                Action::InflightCrash => self.crash_inflight(env),
                Action::OldPin => {
                    let object = 2 + self.rng.pick(6);
                    let node = self.rng.pick(self.nodes.len());
                    let warm = self.request_on(object, false, false, node);
                    self.exchange(warm, false);
                    let client = self.request_on(object, true, false, node);
                    self.update(object);
                    self.exchange(client, false);
                    self.coverage.action("old-pin");
                }
                Action::FailedDirtyWrite => {
                    self.sim.inject("writev", Fault::Errno(libc::EIO)).unwrap();
                    *self.coverage.injected.entry("writev".into()).or_default() += 1;
                    self.update(4);
                    let client = self.request(4, false, false);
                    self.exchange(client, false);
                    self.coverage.action("failed-dirty-write");
                }
                Action::InflightMembership if self.nodes.len() < MAX_NODES => {
                    let mut client = self.request(1, false, false);
                    self.tick();
                    let complete = client.poll();
                    self.add(env, None);
                    if complete {
                        self.check(&client, false);
                        client.fd.take();
                        self.settle();
                    } else {
                        self.exchange(client, false);
                    }
                    self.coverage.action("inflight-membership");
                }
                Action::Partition if self.nodes.len() > 1 => self.partition_traffic(),
                Action::WallJump => {
                    let wall = env.ctx.clock.wall_now();
                    let amount = Duration::from_secs(1 + self.rng.pick(120) as u64);
                    self.clock.set_wall_time(if self.rng.pick(2) == 0 {
                        wall + amount
                    } else {
                        wall - amount
                    });
                    self.traffic(1, true);
                    // Recover past replay admission's wall high-water mark.
                    self.clock.advance(Duration::from_secs(121));
                    let (anchor, wall_anchor) = env.ctx.clock.anchor();
                    self.clock
                        .set_wall_time(wall_anchor + env.ctx.now().duration_since(anchor));
                    self.coverage.action("wall-jump");
                }
                Action::OriginFault => self.origin_fault(),
                Action::MalformedClient => self.malformed_client(),
                Action::KeyRetirement => self.key_retirement(),
                Action::CacheRecreate => self.cache_recreate(env),
                Action::NativeFault if self.native => self.native_fault(env),
                Action::PeerSecurity if self.nodes.len() > 1 => self.peer_security(env),
                Action::DiskCorruption => self.disk_corruption(),
                Action::PendingWriteCrash => self.crash_pending_write(env),
                // Do not resample gated actions: preserve weights and random draws.
                Action::Traffic
                | Action::AddNode
                | Action::RemoveNode
                | Action::InflightMembership
                | Action::Partition
                | Action::NativeFault
                | Action::PeerSecurity => {
                    let count = 1 + self.rng.pick(4);
                    self.coverage.action("traffic");
                    self.traffic(count, false);
                }
            }
        }
        fn verify_recovery(&mut self) {
            for object in 0..8 {
                let client = self.request(object, false, false);
                self.exchange(client, false);
            }
            for node in &self.nodes {
                for worker in &node.workers {
                    let snapshot = worker.app.store.writer.index().snapshot().unwrap();
                    assert!(snapshot.entries.len() <= node.config.limits.metadata_entries.get());
                    self.coverage.persisted += snapshot.entries.len();
                }
            }
            self.cache_obligations();
            self.coverage.origin_gets = self.catalog.borrow().gets;
            self.coverage.origin_faults = self.catalog.borrow().faults.clone();
            for (operation, injected) in &self.coverage.injected {
                assert_eq!(
                    self.coverage.observed.get(operation),
                    Some(injected),
                    "injected OS fault was not consumed"
                );
            }
        }
        fn teardown_generated(&mut self, env: &racer_io::Env) {
            while !self.nodes.is_empty() {
                self.remove(env, 0);
            }
            self.coverage.collect(&self.sim);
            self.coverage.collect_native(&self.fabric);
            assert_eq!(
                self.sim.live_handles(),
                0,
                "all descriptors must be fenced and released"
            );
            assert_eq!(
                self.fabric.live_resources(),
                0,
                "native resources must be fenced and released"
            );
            self.coverage.trace.record(format!(
                "final-invariants:{}:{}:{}:{}:{}:{}",
                self.sim.live_handles(),
                self.fabric.live_resources(),
                self.coverage.success,
                self.coverage.failures,
                self.coverage.bytes,
                self.coverage.persisted
            ));
            self.coverage.trace.checkpoint();
            eprintln!("dst seed={} coverage={:?}", self.seed, self.coverage);
        }
    }
}

#[derive(Clone, Copy, Debug)]
enum Action {
    AddNode,
    RemoveNode,
    Update,
    Evict,
    Restart,
    ShortIo,
    ConnectFailure,
    DelayedWrite,
    ClientCancel,
    PeerOutage,
    InflightCrash,
    OldPin,
    FailedDirtyWrite,
    InflightMembership,
    Traffic,
    Partition,
    WallJump,
    OriginFault,
    MalformedClient,
    KeyRetirement,
    CacheRecreate,
    NativeFault,
    PeerSecurity,
    DiskCorruption,
    PendingWriteCrash,
}

// Repeated entries are weights. Keep the original bag order so swap_remove and
// seeded selection preserve the action schedule and random draws.
const WEIGHTED_ACTIONS: &[Action] = &[
    Action::AddNode,
    Action::RemoveNode,
    Action::Update,
    Action::Evict,
    Action::Restart,
    Action::ShortIo,
    Action::ConnectFailure,
    Action::DelayedWrite,
    Action::ClientCancel,
    Action::PeerOutage,
    Action::InflightCrash,
    Action::OldPin,
    Action::FailedDirtyWrite,
    Action::InflightMembership,
    // Former IDs 14-17 all selected ordinary traffic.
    Action::Traffic,
    Action::Traffic,
    Action::Traffic,
    Action::Traffic,
    Action::Partition,
    Action::WallJump,
    Action::OriginFault,
    Action::MalformedClient,
    Action::KeyRetirement,
    Action::CacheRecreate,
    Action::NativeFault,
    Action::PeerSecurity,
    Action::DiskCorruption,
    Action::PendingWriteCrash,
    Action::OriginFault,
    Action::OriginFault,
    Action::OriginFault,
    Action::OriginFault,
    Action::PeerSecurity,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
    Action::NativeFault,
];

const CLASSES: [ResourceClass; 11] = [
    ResourceClass::Plaintext,
    ResourceClass::Ciphertext,
    ResourceClass::DirtyCiphertext,
    ResourceClass::Registered,
    ResourceClass::RequestContext,
    ResourceClass::Flight,
    ResourceClass::Waiter,
    ResourceClass::Connection,
    ResourceClass::Pipe,
    ResourceClass::ControlProgress,
    ResourceClass::Relay,
];

/// SplitMix64 is local to action generation; cryptographic entropy is a separate stream.
struct Random(u64);
impl Random {
    fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9e3779b97f4a7c15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xbf58476d1ce4e5b9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94d049bb133111eb);
        z ^ (z >> 31)
    }
    fn pick(&mut self, n: usize) -> usize {
        (self.next() % n as u64) as usize
    }
}

#[derive(Clone)]
struct Version {
    tag: String,
    bytes: Vec<u8>,
}
#[derive(Default)]
struct Catalog {
    current: BTreeMap<usize, Version>,
    calls: usize,
    gets: usize,
    fault: Option<OriginFault>,
    faults: BTreeMap<&'static str, usize>,
}
#[derive(Clone, Copy, Debug)]
enum OriginFault {
    Reject,
    Forbidden,
    DuplicateLength,
    Truncate,
    WrongEtag,
}
#[derive(Default, Debug, PartialEq, Eq)]
struct Coverage {
    actions: BTreeMap<&'static str, usize>,
    operations: BTreeMap<String, usize>,
    injected: BTreeMap<String, usize>,
    observed: BTreeMap<String, usize>,
    success: usize,
    failures: usize,
    bytes: usize,
    persisted: usize,
    relay_turns: usize,
    secondary_worker_turns: usize,
    native_faults: BTreeMap<String, usize>,
    native_writes: usize,
    origin_gets: usize,
    origin_faults: BTreeMap<&'static str, usize>,
    trace: ReplayTrace,
}

/// Ordered streaming digest plus bounded diagnostic history. No host addresses,
/// debug Instants, or allocator identities enter this normalized trace. Resource
/// IDs are the simulators' world-local IDs and are intentionally NOT reordered.
struct ReplayTrace {
    hash: Sha256,
    events: u64,
    recent: VecDeque<String>,
    checkpoints: Vec<(u64, [u8; 32])>,
}
impl std::fmt::Debug for ReplayTrace {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReplayTrace")
            .field("events", &self.events)
            .field(
                "digest",
                &format_args!("{:x}", self.hash.clone().finalize()),
            )
            .field("recent", &self.recent)
            .finish()
    }
}
impl Default for ReplayTrace {
    fn default() -> Self {
        Self {
            hash: Sha256::new(),
            events: 0,
            recent: VecDeque::new(),
            checkpoints: Vec::new(),
        }
    }
}
impl PartialEq for ReplayTrace {
    fn eq(&self, other: &Self) -> bool {
        self.events == other.events
            && self.digest() == other.digest()
            && self.checkpoints == other.checkpoints
    }
}
impl Eq for ReplayTrace {}

fn handle(fd: &Descriptor) -> racer_io::sim::Handle {
    fd.as_sim().expect("DST escaped into host I/O")
}
fn would_block(error: &std::io::Error) -> bool {
    error.kind() == std::io::ErrorKind::WouldBlock
}
fn key(object: usize) -> String {
    format!("{object:064x}")
}
fn node_id(id: usize) -> NodeId {
    NodeId(format!("{id:08x}-1111-4111-8111-111111111111"))
}
fn cache(id: usize) -> CacheDefinition {
    let name = format!("dst-{id}");
    let (client_socket, origin_socket) =
        racer_wire::control::canonical_socket_paths(&name).unwrap();
    CacheDefinition {
        name,
        id: CacheId("33333333-3333-4333-8333-333333333333".into()),
        client_socket,
        origin_socket,
        priority: racer_wire::control::DEFAULT_PRIORITY,
        mode: racer_wire::control::CacheMode::Cached,
        origin: None,
    }
}

/// Bounded raw HTTP fixture. It deliberately does not use Racer's HTTP codec,
/// range normalization, metadata structs, read service, or crypto implementation.
struct OriginConnection {
    fd: Descriptor,
    input: Vec<u8>,
    output: Vec<u8>,
    sent: usize,
}
struct Adapter {
    listener: Descriptor,
    connections: Vec<OriginConnection>,
}
impl Adapter {
    fn new(sim: &SimOs, id: usize) -> Self {
        let path = cache(id).origin_socket;
        sim.create_dir_all(path.parent().unwrap()).unwrap();
        let _ = sim.unlink(&path);
        Self {
            listener: sim.listen(SocketAddress::Unix(path)).unwrap(),
            connections: vec![],
        }
    }
    fn poll(&mut self, catalog: &Rc<RefCell<Catalog>>) {
        match handle(&self.listener).accept() {
            Ok(fd) => self.connections.push(OriginConnection {
                fd,
                input: vec![],
                output: vec![],
                sent: 0,
            }),
            Err(e) if would_block(&e) => (),
            Err(e) => panic!("origin accept: {e}"),
        }
        self.connections.retain_mut(|c| {
            if c.output.is_empty() {
                let mut bytes = [0; 8192];
                match handle(&c.fd).recv(&mut bytes) {
                    Ok(0) => return false,
                    Ok(n) => c.input.extend_from_slice(&bytes[..n]),
                    Err(e) if would_block(&e) => return true,
                    Err(_) => return false,
                }
                assert!(c.input.len() <= 32768, "unbounded origin head");
                if !c.input.ends_with(b"\r\n\r\n") {
                    return true;
                }
                c.output = origin_response(&c.input, &mut catalog.borrow_mut());
            }
            match handle(&c.fd).send(&c.output[c.sent..]) {
                Ok(n) => c.sent += n,
                Err(e) if would_block(&e) => (),
                Err(_) => return false,
            }
            c.sent != c.output.len()
        });
    }
}

fn headers(bytes: &[u8]) -> (String, BTreeMap<String, String>, usize) {
    let end = bytes
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .expect("complete HTTP head")
        + 4;
    let text = std::str::from_utf8(&bytes[..end]).unwrap();
    let mut lines = text.split("\r\n");
    let start = lines.next().unwrap().to_owned();
    let mut fields = BTreeMap::new();
    for line in lines.filter(|l| !l.is_empty()) {
        let (name, value) = line.split_once(": ").expect("canonical HTTP field");
        assert!(
            fields
                .insert(name.to_ascii_lowercase(), value.into())
                .is_none(),
            "duplicate field"
        );
    }
    (start, fields, end)
}
fn origin_response(bytes: &[u8], catalog: &mut Catalog) -> Vec<u8> {
    let (start, h, _) = headers(bytes);
    let mut parts = start.split_whitespace();
    let method = parts.next().unwrap();
    let object = usize::from_str_radix(
        parts.next().unwrap().strip_prefix("/v1/objects/").unwrap(),
        16,
    )
    .unwrap();
    assert_eq!(h.get("host").map(String::as_str), Some("racer"));
    assert_eq!(
        h.get("racer-metadata").map(String::as_str),
        Some("dst opaque metadata")
    );
    assert_eq!(
        h.get("authorization").map(String::as_str),
        Some("Bearer dst-fixture")
    );
    catalog.calls += 1;
    if let Some(fault) = catalog.fault.take() {
        let (name, status) = match fault {
            OriginFault::Reject => ("credential-reject", 401),
            OriginFault::Forbidden => ("credential-forbidden", 403),
            OriginFault::DuplicateLength => ("origin-duplicate-length", 0),
            OriginFault::Truncate => ("origin-truncated-body", 0),
            OriginFault::WrongEtag => ("origin-malformed-etag", 0),
        };
        *catalog.faults.entry(name).or_default() += 1;
        if status != 0 {
            return format!(
                "HTTP/1.1 {status} Rejected\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            )
            .into_bytes();
        }
        let mut response = origin_response(bytes, catalog);
        match fault {
            OriginFault::DuplicateLength => (),
            OriginFault::Truncate => {
                response.pop();
            }
            OriginFault::WrongEtag => {
                if let Some(at) = response.windows(6).position(|w| w == b"ETag: ") {
                    response[at + 6] = b'W';
                }
            }
            _ => unreachable!(),
        }
        if matches!(fault, OriginFault::DuplicateLength) {
            let at = response.windows(4).position(|w| w == b"\r\n\r\n").unwrap();
            response.splice(at..at, b"\r\nContent-Length: 999".iter().copied());
        }
        return response;
    }
    let Some(version) = catalog.current.get(&object) else {
        return b"HTTP/1.1 404 Missing\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec();
    };
    if h.get("if-match").is_some_and(|tag| tag != &version.tag) {
        return b"HTTP/1.1 412 Gone\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec();
    }
    let size = version.bytes.len();
    let (status, first, end) = if method == "HEAD" || size == 0 {
        (200, 0, size)
    } else {
        let (first, last) = h["range"]
            .strip_prefix("bytes=")
            .unwrap()
            .split_once('-')
            .unwrap();
        let first: usize = first.parse().unwrap();
        let last: usize = last.parse().unwrap();
        assert_eq!(first % PAGE_BYTES as usize, 0);
        assert!(last == first + PAGE_BYTES as usize - 1 || last + 1 == size);
        if first >= size {
            return format!("HTTP/1.1 416 Range\r\nContent-Length: 0\r\nContent-Range: bytes */{size}\r\nConnection: close\r\n\r\n").into_bytes();
        }
        (206, first, (last + 1).min(size))
    };
    let range = if status == 206 {
        format!("Content-Range: bytes {first}-{}/{size}\r\n", end - 1)
    } else {
        String::new()
    };
    let mut response = format!("HTTP/1.1 {status} Result\r\nContent-Length: {}\r\nContent-Type: application/octet-stream\r\nETag: {}\r\nRacer-Expires-At: 0\r\n{range}Connection: close\r\n\r\n", end - first, version.tag).into_bytes();
    if method == "GET" {
        catalog.gets += 1;
        response.extend_from_slice(&version.bytes[first..end]);
    }
    response
}

struct Node {
    id: usize,
    config: Config,
    workers: Vec<LocalWorker>,
    adapter: Adapter,
    control: Option<Rc<Session>>,
}
/// Page crypto plus, for the first worker built, the node's RDMA owner, both
/// stepped on the harness thread. Production runs the owner on its own
/// thread; this harness is single-threaded and deterministic.
struct SteppedCrypto {
    engine: crate::security::PageCryptoEngine,
    owner: Option<rdma_verbs::Owner>,
}

impl crate::lifecycle::Service<RequestScope> for SteppedCrypto {
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.engine.start(scope)
    }

    fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
        self.engine.poll_budgeted(cx, budget)?;
        if let Some(owner) = &mut self.owner {
            owner.poll_budgeted(budget)?;
        }
        Ok(())
    }

    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.engine.drain(scope)
    }

    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.engine.shutdown(scope)
    }
}

fn stepped_crypto(
    node: &NodeState,
    engine: crate::security::PageCryptoEngine,
) -> Box<dyn crate::lifecycle::Service<RequestScope>> {
    let owner = node.native.take_owner(engine.ctx().clone());
    Box::new(SteppedCrypto { engine, owner })
}

struct LocalWorker {
    app: WorkerApplication,
    runtime: WorkerRuntime,
    crypto: Box<dyn crate::lifecycle::Service<RequestScope>>,
}
impl Node {
    /// Steps every worker once; returns the completions reaped.
    fn poll(&mut self, budget: usize) -> usize {
        let mut completions = 0;
        for worker in &mut self.workers {
            if worker.app.control.is_some() {
                // This harness supplies accepted publications and keys directly.
                // Keep listener-owner semantics without starting either live feed.
                worker.app.control_task = Some(Box::pin(std::future::pending()));
                worker.app.keyring_task = Some(Box::pin(std::future::pending()));
            }
            let _local = worker
                .app
                .directory
                .simulation_scope(Some((worker.app.worker, worker.app.coordinator.clone())));
            completions += worker.runtime.reactor.poll_budgeted(budget).unwrap();
            worker
                .crypto
                .poll_budgeted(&mut Context::from_waker(&wake::waker()), budget)
                .unwrap();
            worker.runtime.crypto.poll_budgeted(budget).unwrap();
            worker
                .app
                .poll_budgeted(&mut Context::from_waker(&wake::waker()), budget)
                .unwrap();
            for class in CLASSES {
                assert!(
                    worker.runtime.admission.used(class) <= worker.runtime.admission.limit(class),
                    "node {} resource bound",
                    self.id
                );
            }
            assert!(
                worker.app.store.writer.pending_count() <= self.config.limits.queue_entries.get()
            );
            assert!(
                worker.app.clients.active_connections()
                    <= self.config.limits.client_connections.get()
            );
        }
        completions
    }
}

struct Harness {
    /// Shared publication retention selected before constructing any worker graph.
    retained_snapshots: usize,

    seed: u64,
    rng: Random,
    sim: SimOs,
    nodes: Vec<Node>,
    next_node: usize,
    generation: u64,
    catalog: Rc<RefCell<Catalog>>,
    oracle: BTreeMap<(usize, String), Vec<u8>>,
    revisions: BTreeMap<usize, u64>,
    coverage: Coverage,
    ca: rcgen::Certificate,
    ca_key: rcgen::KeyPair,
    clock: racer_io::SimClock,
    fabric: rdma_verbs::simulation::Simulation,
    native: bool,
    key_epoch: u8,
    cache_epoch: u64,
    origin_faults: Vec<OriginFault>,
    security_faults: Vec<bool>,
    native_rules: Vec<(
        rdma_verbs::simulation::Operation,
        rdma_verbs::simulation::Fault,
    )>,
    payload_regression: bool,
    opaque_relay: bool,
    concurrent_layers: bool,
}
impl Harness {
    fn new(seed: u64, sim: SimOs, clock: racer_io::SimClock, native: bool) -> Self {
        let key = racer_crypto::SigningKey::from_seed(&[91; 32])
            .to_pkcs8_der()
            .unwrap();
        let ca_key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(key.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let ca = params.self_signed(&ca_key).unwrap();
        Self {
            retained_snapshots: 64,
            seed,
            rng: Random(seed),
            sim,
            nodes: vec![],
            next_node: 0,
            generation: 0,
            catalog: Rc::new(RefCell::new(Catalog::default())),
            oracle: BTreeMap::new(),
            revisions: BTreeMap::new(),
            coverage: Coverage::default(),
            ca,
            ca_key,
            clock,
            fabric: rdma_verbs::simulation::Simulation::new(),
            native,
            key_epoch: 0,
            cache_epoch: 0,
            origin_faults: vec![],
            security_faults: vec![],
            native_rules: vec![],
            payload_regression: false,
            opaque_relay: false,
            concurrent_layers: false,
        }
    }
    fn update(&mut self, object: usize) {
        let revision = self.revisions.entry(object).or_default();
        *revision += 1;
        let length = match object {
            1..=4 if self.concurrent_layers => 4 * PAGE_BYTES as usize + 257,
            0 => 0,
            1 if self.payload_regression => 4 * PAGE_BYTES as usize + 257,
            1 => PAGE_BYTES as usize + 257,
            _ => 1 + self.rng.pick(16384),
        };
        let mut random =
            Random(self.seed ^ (object as u64).rotate_left(17) ^ revision.rotate_left(37));
        let bytes: Vec<u8> = (0..length).map(|_| random.next() as u8).collect();
        let tag = format!("\"object-{object}-v{revision}\"");
        self.coverage.trace.record(format!(
            "update:{object}:{revision}:{length}:{:x}",
            Sha256::digest(&bytes)
        ));
        self.oracle.insert((object, tag.clone()), bytes.clone());
        self.catalog
            .borrow_mut()
            .current
            .insert(object, Version { tag, bytes });
        self.coverage.action("update");
    }
    fn identity(&self, config: &Config, id: usize) -> Arc<SigningIdentity> {
        let mut random = Random(self.seed ^ id as u64);
        let seed = std::array::from_fn(|_| random.next() as u8);
        let key = racer_crypto::SigningKey::from_seed(&seed)
            .to_pkcs8_der()
            .unwrap();
        let pending = PendingIdentity::recover(&key).unwrap();
        signing_identity(
            pending,
            &self.ca,
            &self.ca_key,
            config.cluster.clone(),
            config.node.clone(),
        )
    }
    fn add(&mut self, env: &racer_io::Env, restart: Option<usize>) {
        let id = restart.unwrap_or_else(|| {
            let id = self.next_node;
            self.next_node += 1;
            id
        });
        // Restart is a new process incarnation, not a rewind of nonce entropy.
        let role = self.clock.ctx(1 + id as u64 + (self.generation << 32));
        let env = racer_io::Env::new(env.driver.clone(), role.clone());
        let device = format!("dst-rnic-{id}");
        let gid = [1 + (id % 254) as u8; 16];
        let fabric = self
            .fabric
            .with_devices(vec![rdma_verbs::simulation::Device::new(
                device.clone(),
                gid,
            )])
            .unwrap();
        let _fabric = fabric.enter();
        let mut config = cluster::config(self.native);
        config.opaque_relay = self.opaque_relay;
        // Memory pressure reads the real host cgroup and PSI files, which the
        // simulation does not control. Turn it off so replays stay deterministic.
        config.memory_pressure = false;
        config.node = node_id(id);
        config.peer_listen = format!("127.0.0.1:{}", 20000 + id).parse().unwrap();
        config.diagnostics_listen = format!("127.0.0.1:{}", 30000 + id).parse().unwrap();
        config.slab_directory = format!("/dst/node-{id}/slabs").into();
        config.slab_bytes = 256 * 1024 * 1024;
        config.free_segment_reserve = 1;
        config.limits.queue_entries = NonZeroUsize::new(64).unwrap();
        config.limits.client_connections = NonZeroUsize::new(64).unwrap();
        config.limits.connections_per_neighbor = NonZeroUsize::new(2).unwrap();
        config.limits.range_window_pages = NonZeroUsize::new(2).unwrap();
        config.limits.metadata_entries = NonZeroUsize::new(128).unwrap();
        config.limits.retained_snapshots = NonZeroUsize::new(self.retained_snapshots).unwrap();
        // Match one quarter of the production node budget, including peer envelope
        // staging. This fixture assembles workers directly, bypassing size_workers.
        config.limits.request_context_bytes = NonZeroUsize::new(16 * 1024 * 1024).unwrap();
        if self.payload_regression {
            config.limits.plaintext_bytes = NonZeroUsize::new(64 * 1024 * 1024).unwrap();
            config.limits.ciphertext_bytes = NonZeroUsize::new(64 * 1024 * 1024).unwrap();
        }
        if self.concurrent_layers {
            config.limits.plaintext_bytes = NonZeroUsize::new(256 * 1024 * 1024).unwrap();
            config.limits.ciphertext_bytes = NonZeroUsize::new(256 * 1024 * 1024).unwrap();
            config.limits.client_connections = NonZeroUsize::new(512).unwrap();
            config.limits.connections_per_neighbor = NonZeroUsize::new(16).unwrap();
        }
        let worker_count = 1 + self.rng.pick(2);
        self.coverage.trace.record(format!(
            "node:{id}:{restart:?}:{worker_count}:{}",
            self.generation
        ));
        let worker_ids: Vec<_> = (0..worker_count).map(|i| WorkerId(i as u16)).collect();
        let mut state = NodeState::new(worker_ids.clone(), 64, env.ctx.clone()).unwrap();
        // Retention belongs to the shared publication authority, not worker handles.
        state.publications = Arc::new(crate::control::cell::PublicationCell::new(
            crate::control::Snapshot::retention(config.limits.retained_snapshots.get()),
        ));
        let node = Arc::new(state);
        if self.native {
            node.native
                .prepare(worker_ids.into_iter(), &config.limits)
                .unwrap();
        }
        let discovered_nics = if self.native {
            vec![racer_wire::control::RailMapping {
                rail: racer_wire::control::RailId(0),
                numa_node: None,
                device,
                port: 1,
                gid: Some(gid),
            }]
        } else {
            Vec::new()
        };
        let (mut app, runtime, engine) =
            local_worker_with_fabric(&env, &config, &node, 0, discovered_nics);
        let crypto = stepped_crypto(&node, engine);
        app.keys
            .install(self.bundle(&config, u64::from(self.key_epoch) + 1))
            .unwrap();
        app.keys
            .install_signing_identity(self.identity(&config, id))
            .unwrap();
        app.telemetry
            .attach_io(runtime.reactor.clone(), runtime.admission.clone())
            .unwrap();
        // The harness supplies accepted control inputs; startup/recovery and every
        // datapath dependency remain the production application implementation.
        let control = app.control.take();
        self.generation += 1;
        let mut publication = publication(&config, self.generation, vec![self.definition(id)]);
        publication.membership_version = MembershipVersion(self.generation);
        publication.members = self.members().into_iter().map(Into::into).collect();
        publication.members.push(member(&config).into());
        app.snapshots.apply(publication).unwrap();
        let startup = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
        let mut workers = vec![LocalWorker {
            app,
            runtime,
            crypto,
        }];
        for worker in 1..worker_count {
            let role = self
                .clock
                .ctx(1 + id as u64 + (self.generation << 32) + ((worker as u64) << 48));
            let env = racer_io::Env::new(env.driver.clone(), role.clone());
            let (app, runtime, engine) = local_worker_with_fabric(
                &env,
                &config,
                &node,
                worker as u16,
                workers[0].app.discovered_nics.clone(),
            );
            let crypto = stepped_crypto(&node, engine);
            workers.push(LocalWorker {
                app,
                runtime,
                crypto,
            });
        }
        lifecycle(&mut workers, &startup, Lifecycle::Start);
        self.persist_provisioning(id, &config, &workers);
        if self.native {
            // Rails are node-wide: the owner's worker activates them for all.
            assert!(
                !workers[0].app.actual_rails.is_empty()
                    && workers
                        .iter()
                        .all(|w| !w.app.links.as_ref().unwrap().active().is_empty()),
                "native activation must not silently fall back"
            );
        }
        if worker_count > 1 {
            self.coverage.action("multi-worker");
        }
        self.start_node_listeners(&env, id, &config, &mut workers[0], &startup);
        let adapter = Adapter::new(&self.sim, id);
        self.nodes.push(Node {
            id,
            config,
            workers,
            adapter,
            control,
        });
        self.publish();
        self.tick();
        self.coverage
            .action(if restart.is_some() { "restart" } else { "add" });
    }

    fn persist_provisioning(&self, id: usize, config: &Config, workers: &[LocalWorker]) {
        // Persist provisioning, not cached page writes. Every ancestor binding is
        // explicit so a power loss cannot erase the fixture's whole node directory.
        for path in [
            std::path::PathBuf::from("/"),
            "/dst".into(),
            format!("/dst/node-{id}").into(),
            config.slab_directory.clone(),
        ] {
            self.sim.disk().sync(&path).unwrap();
        }
        for worker in workers {
            self.sim
                .disk()
                .sync(
                    &config
                        .slab_directory
                        .join(format!("worker-{}-slab-0.dat", worker.app.worker.0)),
                )
                .unwrap();
        }
    }

    fn start_node_listeners(
        &self,
        env: &racer_io::Env,
        id: usize,
        config: &Config,
        worker: &mut LocalWorker,
        startup: &RequestScope,
    ) {
        let LocalWorker {
            app,
            runtime,
            crypto,
        } = worker;
        drive_local(
            runtime,
            &mut **crypto,
            app.clients.reconcile(&[self.definition(id)], startup),
        )
        .unwrap();
        let peers = app.peers.clone();
        let address = config.peer_listen;
        let listener_scope = scope(&env.ctx.clock, Duration::from_secs(365 * 24 * 3600)).unwrap();
        let peer_scope = listener_scope.clone();
        app.listener_scope = Some(listener_scope);
        app.peer_task = Some(Box::pin(
            async move { peers.listen(address, &peer_scope).await },
        ));
    }
    fn members(&self) -> Vec<crate::topology::Member> {
        self.nodes.iter().map(|n| member(&n.config)).collect()
    }
    fn definition(&self, id: usize) -> CacheDefinition {
        let mut definition = cache(id);
        definition.id = CacheId(format!("33333333-3333-4333-8333-{:012x}", self.cache_epoch));
        definition
    }
    fn bundle(&self, config: &Config, _generation: u64) -> wire::KeyringBundle {
        wire::KeyringBundle {
            schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(u64::from(self.key_epoch) + 1),
            peer_trust_roots: vec![self.ca.der().to_vec()],
            cache_keys: [
                wire::CacheKeyPurpose::Page,
                wire::CacheKeyPurpose::OriginCredentials,
            ]
            .into_iter()
            .enumerate()
            .map(|(i, purpose)| {
                wire::CacheEncryptionKey::new(
                    wire::CacheKeyRef {
                        cache: self.definition(0).id,
                        id: crate::model::key_id_from_generation(
                            u64::from(self.key_epoch) + 1,
                            7 + i as u32 + u32::from(self.key_epoch) * 2,
                        )
                        .unwrap(),
                        purpose,
                    },
                    wire::CacheKeyState::Active,
                    zeroize::Zeroizing::new([19 + i as u8 + self.key_epoch * 2; 32]),
                )
            })
            .collect(),
        }
    }
    fn publish(&mut self) {
        self.generation += 1;
        let members = self.members();
        let definitions: Vec<_> = self.nodes.iter().map(|n| self.definition(n.id)).collect();
        for (node, definition) in self.nodes.iter_mut().zip(definitions) {
            let mut p = publication(&node.config, self.generation, vec![definition]);
            p.membership_version = MembershipVersion(self.generation);
            p.members = members.iter().cloned().map(Into::into).collect();
            node.workers[0].app.snapshots.apply(p).unwrap();
        }
    }
    /// Advances 1 ms and steps every node once; returns completions reaped.
    fn tick(&mut self) -> usize {
        let mut completions = 0;
        self.clock.advance(Duration::from_millis(1));
        let start = self.rng.pick(self.nodes.len());
        let budget = 1 + self.rng.pick(64);
        self.coverage.trace.record(format!(
            "tick:{}:{start}:{budget}",
            self.clock.elapsed().as_nanos()
        ));
        for offset in 0..self.nodes.len() {
            let index = (start + offset) % self.nodes.len();
            let _endpoint = self
                .sim
                .enter_endpoint(SocketAddress::Inet(self.nodes[index].config.peer_listen));
            self.nodes[index].adapter.poll(&self.catalog);
            completions += self.nodes[index].poll(budget);
            for worker in &self.nodes[index].workers {
                let used: Vec<_> = CLASSES
                    .iter()
                    .map(|class| worker.runtime.admission.used(*class))
                    .collect();
                self.coverage.trace.record(format!(
                    "invariant:{}:{}:{used:?}:{}:{}:{}:{}",
                    self.nodes[index].id,
                    worker.app.worker.0,
                    worker.app.drivers.pending(),
                    worker.runtime.crypto.outstanding(),
                    worker.runtime.reactor.in_flight(),
                    worker.app.store.writer.pending_count()
                ));
            }
            self.coverage.secondary_worker_turns += self.nodes[index]
                .workers
                .iter()
                .skip(1)
                .filter(|w| {
                    w.runtime.crypto.outstanding() != 0
                        || w.app.drivers.pending() != 0
                        || w.app.store.writer.pending_count() != 0
                })
                .count();
            self.coverage.relay_turns += usize::from(
                self.nodes[index].workers[0]
                    .runtime
                    .admission
                    .used(ResourceClass::Relay)
                    > 0,
            );
        }
        self.coverage.collect(&self.sim);
        self.coverage.collect_native(&self.fabric);
        assert!(
            self.sim.live_handles()
                <= self.nodes.iter().map(|n| n.workers.len()).sum::<usize>() * 256 + 32,
            "descriptor bound"
        );
        completions
    }
    fn remove(&mut self, env: &racer_io::Env, index: usize) -> usize {
        self.retire(env, index, false)
    }
    fn retire(&mut self, env: &racer_io::Env, index: usize, crash: bool) -> usize {
        let mut node = self.nodes.remove(index);
        self.coverage
            .trace
            .record(format!("retire:{}:{crash}", node.id));
        let shutdown = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
        if crash {
            self.sim
                .disk()
                .crash_under(std::path::Path::new(&format!("/dst/node-{}", node.id)))
                .unwrap();
            node.workers[0].app.directory.simulation_crash();
            for worker in &mut node.workers {
                if let Some(endpoint) = &mut worker.app.endpoint {
                    endpoint.simulation_crash();
                }
                worker.app.drivers.simulation_crash();
            }
            node.workers[0].app.directory.simulation_crash();
        } else {
            lifecycle(&mut node.workers, &shutdown, Lifecycle::Drain);
            // Production checkpoint publication promises an atomic logical cut,
            // not fsync durability (store/checkpoint.rs). Power-loss actions may
            // therefore lose it; never assert durability merely from shutdown.
            lifecycle(&mut node.workers, &shutdown, Lifecycle::Shutdown);
        }
        let id = node.id;
        let Node {
            workers,
            adapter,
            control,
            ..
        } = node;
        drop((adapter, control));
        let mut admissions = Vec::new();
        for LocalWorker {
            app,
            runtime,
            mut crypto,
        } in workers
        {
            let _local = app
                .directory
                .simulation_scope(Some((app.worker, app.coordinator.clone())));
            let drivers = app.drivers.clone();
            let directory = app.directory.clone();
            // The simulated process is gone even though this oracle retains its
            // admission counters. Prevent final payload owners from recycling
            // into a dead process's idle pool while completion fences run.
            runtime.admission.stop();
            drop(app);
            if crash {
                drivers.simulation_crash();
                directory.simulation_crash();
                // This is only the OS cancellation fence after graph destruction,
                // not WorkerApplication::drain: no acquisition, persistence, or
                // checkpoint producer is polled after the process-loss cut.
                let mut fence = runtime.reactor.drain();
                let mut done = false;
                for _ in 0..MAX_TURNS {
                    runtime.reactor.poll_budgeted(64).unwrap();
                    if let Poll::Ready(result) = fence
                        .as_mut()
                        .poll(&mut Context::from_waker(&wake::waker()))
                    {
                        result.unwrap();
                        done = true;
                        break;
                    }
                }
                assert!(done, "crash kernel fence stalled");
            } else {
                drive_local(&runtime, &mut *crypto, runtime.reactor.drain()).unwrap();
                assert_eq!(runtime.crypto.outstanding(), 0);
            }
            let admission = runtime.admission.clone();
            drop((drivers, directory));
            drop((runtime, crypto));
            admissions.push(admission);
        }
        for admission in admissions {
            let used: Vec<_> = CLASSES.iter().map(|class| admission.used(*class)).collect();
            self.coverage
                .trace
                .record(format!("retired-invariants:{id}:{used:?}"));
            for class in CLASSES {
                assert_eq!(
                    admission.used(class),
                    0,
                    "retired node {id}, class {class:?}"
                );
            }
        }
        self.publish();
        self.coverage.action(if crash { "crash" } else { "remove" });
        id
    }
    fn settle(&mut self) {
        let mut idle = 0;
        for _ in 0..MAX_TURNS {
            let completions = self.tick();
            let woken = wake::take();
            let in_flight: usize = self
                .nodes
                .iter()
                .flat_map(|n| &n.workers)
                .map(|w| w.runtime.reactor.in_flight() + w.runtime.crypto.outstanding())
                .sum();
            idle = if completions == 0 && !woken && in_flight == 0 {
                idle + 1
            } else {
                0
            };
            assert!(
                idle < STRANDED_TURNS,
                "seed={} pending work without a wake source for {idle} turns: {:?}",
                self.seed,
                self.coverage
            );
            if self.nodes.iter().flat_map(|n| &n.workers).all(|n| {
                n.app.store.writer.is_idle()
                    && n.app.writer_task.is_none()
                    && n.app.clients.active_connections() == 0
                    && n.runtime.crypto.outstanding() == 0
                    && n.app.drivers.pending() == 0
                    && n.app.links.as_ref().is_none_or(|links| links.setups() == 0)
            }) {
                return;
            }
        }
        panic!("seed={} failed to settle: {:?}", self.seed, self.coverage);
    }

    /// Set up the persistent link from node `a` to node `b` and wait until
    /// both ends are ready, waiting out any backoff. If `a` is ready but `b`
    /// is not (`b` restarted under `a`'s link), `b` initiates instead.
    fn connect_links(&mut self, env: &racer_io::Env, a: usize, b: usize) {
        let rail = racer_wire::control::RailId(0);
        for _ in 0..64 {
            let (local, peer) = (
                self.nodes[a].config.node.clone(),
                self.nodes[b].config.node.clone(),
            );
            let links = self.nodes[a].workers[0].app.links.clone().unwrap();
            let remote = self.nodes[b].workers[0].app.links.clone().unwrap();
            if links.ready(&peer, rail).is_some() && remote.ready(&local, rail).is_some() {
                self.coverage.action("native-link");
                return;
            }
            for state in [links.state(&peer, rail), remote.state(&local, rail)] {
                if let rdma_verbs::LinkState::Backoff(until) = state {
                    self.clock.advance(
                        until.saturating_duration_since(env.ctx.now()) + Duration::from_millis(1),
                    );
                }
            }
            // A crash-restart of `b` leaves `a` holding a half-open `Ready`
            // link that `a` only discovers on use, so `a` never wants a setup.
            // In production the restarted peer sets up the
            // link when it needs one and `a` replaces the stale link; do that.
            let (from, to, target) = if links.ready(&peer, rail).is_some() {
                (b, a, local)
            } else {
                (a, b, peer)
            };
            let endpoint =
                crate::http::Endpoint::Peer(self.nodes[to].config.peer_listen.to_string());
            self.nodes[from].workers[0]
                .app
                .peers
                .transfers()
                .ensure_link(&endpoint, &target, rail);
            self.settle();
        }
        panic!("seed={} link setup stalled", self.seed);
    }

    fn request(&mut self, object: usize, pinned: bool, head: bool) -> Client {
        let node = self.rng.pick(self.nodes.len());
        self.request_on(object, pinned, head, node)
    }
    fn request_on(&mut self, object: usize, pinned: bool, head: bool, node: usize) -> Client {
        let (tag, size) = {
            let catalog = self.catalog.borrow();
            let version = &catalog.current[&object];
            (version.tag.clone(), version.bytes.len())
        };
        let (first, end) = if !pinned {
            (0, size.min(PAGE_BYTES as usize))
        } else if size == 0 {
            (0, 0)
        } else {
            let first = self.rng.pick(size);
            (first, first + 1 + self.rng.pick(size - first))
        };
        let range = if head {
            String::new()
        } else if !pinned {
            format!("Range: bytes=0-{}\r\n", PAGE_BYTES - 1)
        } else if size == 0 {
            "Range: bytes=0-0\r\n".into()
        } else {
            format!("Range: bytes={first}-{}\r\n", end - 1)
        };
        let pin = if pinned {
            format!("If-Match: {tag}\r\n")
        } else {
            String::new()
        };
        let request = format!("{} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{}{pin}{range}Racer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", if head { "HEAD" } else { "POST" }, key(object), if head { "" } else { SUBSCRIPTION_FIELDS }).into_bytes();
        let fd = self
            .sim
            .connect(SocketAddress::Unix(
                cache(self.nodes[node].id).client_socket,
            ))
            .unwrap();
        self.coverage.trace.record(format!(
            "request:{}:{object}:{first}:{end}:{head}:{pinned}:{:x}",
            self.nodes[node].id,
            Sha256::digest(&request)
        ));
        Client {
            frame_cursor: None,
            releases: Vec::new(),
            released: 0,
            fd: Some(fd),
            request,
            sent: 0,
            response: vec![],
            object,
            tag,
            first,
            end,
            size,
            head,
            pinned,
            done: false,
            disconnected: false,
            expected_status: None,
        }
    }

    fn traffic(&mut self, count: usize, faulted: bool) {
        let mut clients = Vec::new();
        for _ in 0..count {
            let object = self.rng.pick(8);
            let pinned = self.rng.pick(2) == 0;
            let head = self.rng.pick(5) == 0;
            clients.push(self.request(object, pinned, head));
        }
        for _ in 0..MAX_TURNS {
            self.tick();
            let start = self.rng.pick(clients.len());
            for offset in 0..clients.len() {
                let index = (start + offset) % clients.len();
                if clients[index].done {
                    continue;
                }
                if clients[index].poll() {
                    self.check(&clients[index], faulted);
                    clients[index].fd.take();
                    clients[index].done = true;
                }
            }
            if clients.iter().all(|c| c.done) {
                self.settle();
                return;
            }
        }
        panic!(
            "seed={} traffic stalled; coverage={:?}",
            self.seed, self.coverage
        );
    }

    fn exchange(&mut self, mut client: Client, faulted: bool) {
        for _ in 0..MAX_TURNS {
            self.tick();
            if client.poll() {
                self.check(&client, faulted);
                client.fd.take();
                self.settle();
                return;
            }
        }
        panic!("seed={} exchange stalled", self.seed);
    }

    fn object_id(&self, object: usize) -> ObjectId {
        ObjectId {
            cache: self.definition(self.nodes[0].id).id,
            key: CacheKey::parse_hex(key(object).as_bytes()).unwrap(),
        }
    }

    fn ranked_nodes(&self, object: &ObjectId) -> Vec<usize> {
        // Use a separate placement cache for fixture setup, not the byte oracle.
        Placement::new(1)
            .rank(
                self.nodes[0].workers[0]
                    .app
                    .snapshots
                    .current()
                    .unwrap()
                    .membership
                    .clone(),
                object,
                PageNumber(0),
            )
            .ordered
            .iter()
            .map(|id| {
                self.nodes
                    .iter()
                    .position(|n| &n.config.node == id)
                    .unwrap()
            })
            .collect()
    }

    fn check(&mut self, client: &Client, faulted: bool) {
        self.coverage.trace.record(format!(
            "client:{}:{}:{}:{}:{}:{}:{}:{faulted}:{}:{:x}",
            client.object,
            client.tag,
            client.first,
            client.end,
            client.head,
            client.pinned,
            client.disconnected,
            client.response.len(),
            Sha256::digest(&client.response)
        ));
        if !client.response.windows(4).any(|w| w == b"\r\n\r\n") {
            assert!(faulted, "healthy client disconnected before headers");
            self.coverage.failures += 1;
            return;
        }
        let (start, headers, end) = headers(&client.response);
        let status: u16 = start.split_whitespace().nth(1).unwrap().parse().unwrap();
        let length: usize = headers["content-length"].parse().unwrap();
        if status >= 400 {
            assert_eq!(length, 0);
            assert_eq!(client.response.len(), end);
            assert!(!headers.contains_key("etag") && !headers.contains_key("racer-expires-at"));
            if status == 416 {
                assert!(client.size == 0 && !client.head);
                assert_eq!(headers["content-range"], "bytes */0");
                self.coverage.action("empty-range");
            } else {
                assert!(
                    (faulted && matches!(status, 401 | 403 | 502 | 503))
                        || client.expected_status == Some(status),
                    "unexpected status {status}: {start}"
                );
                self.coverage.failures += 1;
            }
            return;
        }
        assert_eq!(
            headers["etag"], client.tag,
            "fresh read or pin switched versions"
        );
        assert!(
            client.expected_status.is_none(),
            "malformed client request was accepted"
        );
        let expected = &self.oracle[&(client.object, headers["etag"].clone())];
        assert_eq!(expected.len(), client.size);
        let body = &client.response[end..];
        if client.head {
            assert_eq!(status, 200);
            assert_eq!(length, client.size);
            assert!(body.is_empty());
            assert!(!headers.contains_key("content-range"));
        } else {
            assert_eq!(status, 200);
            assert_eq!(headers["content-type"], "application/octet-stream");
            assert_eq!(headers["racer-range-start"], client.first.to_string());
            assert!(body.len() <= length);
            let mut cursor = 0;
            let mut offset = client.first;
            while cursor + 21 <= body.len() {
                let kind = body[cursor];
                let start =
                    u64::from_be_bytes(body[cursor + 9..cursor + 17].try_into().unwrap()) as usize;
                let count =
                    u32::from_be_bytes(body[cursor + 17..cursor + 21].try_into().unwrap()) as usize;
                cursor += 21;
                if kind == 2 {
                    assert_eq!(offset, client.end);
                    assert_eq!(count, 0);
                    break;
                }
                assert_eq!(kind, 1);
                assert_eq!(start, offset);
                let available = count.min(body.len() - cursor);
                assert_eq!(
                    &body[cursor..cursor + available],
                    &expected[offset..offset + available],
                    "oracle byte mismatch"
                );
                cursor += available;
                offset += available;
                if available != count {
                    break;
                }
            }
            if body.len() != length {
                if !faulted || !client.disconnected {
                    let mut diagnostics = String::new();
                    for node in &self.nodes {
                        for worker in &node.workers {
                            worker
                                .app
                                .telemetry
                                .failures
                                .write(&mut diagnostics)
                                .unwrap();
                        }
                    }
                    panic!(
                        "healthy response truncated: body={} expected={length} offset={offset}: {diagnostics}",
                        body.len()
                    );
                }
                self.coverage.failures += 1;
                return;
            }
        }
        self.coverage.success += 1;
        self.coverage.bytes += if client.head {
            0
        } else {
            client.end - client.first
        };
    }
}

struct Client {
    frame_cursor: Option<usize>,
    releases: Vec<u8>,
    released: usize,
    fd: Option<Descriptor>,
    request: Vec<u8>,
    sent: usize,
    response: Vec<u8>,
    object: usize,
    tag: String,
    first: usize,
    end: usize,
    size: usize,
    head: bool,
    pinned: bool,
    done: bool,
    disconnected: bool,
    expected_status: Option<u16>,
}

pub(super) fn default_grace_staggered_nodes_and_periodic_checkpoint_traffic() {
    let sim = SimOs::new();
    let clock = racer_io::SimClock::new(505);
    let env = racer_io::Env::sim(&sim, clock.ctx(0));
    let mut harness = Harness::new(505, sim, clock, false);
    harness.retained_snapshots = 2;
    for object in 0..8 {
        harness.update(object);
    }
    for _ in 0..4 {
        harness.add(&env, None);
    }
    // Retention is fixed when the node-wide authority is constructed. The shared
    // policy already retains the delayed topology; never replace a worker's view.
    let old = harness.nodes[0].workers[0]
        .app
        .snapshots
        .current()
        .unwrap()
        .membership
        .version;
    let members = harness.members();
    harness.generation += 1;
    for i in 1..4 {
        let node = &harness.nodes[i];
        let mut p = publication(
            &node.config,
            harness.generation,
            vec![harness.definition(node.id)],
        );
        p.membership_version = MembershipVersion(harness.generation);
        p.members = members.iter().cloned().map(Into::into).collect();
        node.workers[0].app.snapshots.apply(p).unwrap();
        assert!(
            node.workers[0]
                .app
                .node
                .publications
                .resolve(old.0, env.ctx.now())
                .unwrap()
                .is_some()
        );
    }
    let client = harness.request_on(2, false, false, 0);
    harness.exchange(client, false);
    harness.clock.advance(Duration::from_secs(6));
    for _ in 0..500 {
        harness.tick();
    }
    for node in &harness.nodes {
        assert!(
            node.workers[0]
                .app
                .telemetry
                .metrics
                .gauge(Gauge::CheckpointSequence)
                > 0
        );
    }
    // A node cannot resolve a future membership it has not received. Complete
    // propagation before requiring arbitrary new-to-old requests to succeed.
    harness.publish();
    harness.traffic(4, false);
}

pub(super) fn dst_origin_recovery_completes_before_concurrent_healthy_reads() {
    for native in [false, true] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(106);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(106, sim, clock, native);
        for object in 0..8 {
            harness.update(object);
        }
        for _ in 0..3 {
            harness.add(&env, None);
        }
        for fault in [
            OriginFault::DuplicateLength,
            OriginFault::Truncate,
            OriginFault::WrongEtag,
            OriginFault::Reject,
            OriginFault::Forbidden,
        ] {
            harness.origin_faults = vec![fault];
            let failures = harness.coverage.failures;
            harness.origin_fault();
            assert!(
                harness.coverage.failures > failures,
                "fault did not fail a read"
            );
            let failures = harness.coverage.failures;
            harness.traffic(8, false);
            assert_eq!(harness.coverage.failures, failures);
        }
        assert_eq!(harness.coverage.actions["origin-recovered"], 5);
        while !harness.nodes.is_empty() {
            harness.remove(&env, 0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

pub(super) fn dst_native_faults_reach_warmed_peers_in_small_topologies() {
    for nodes in [1, 2, 3] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(118);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(118, sim, clock, true);
        for _ in 0..nodes {
            harness.add(&env, None);
        }
        for _ in 0..9 {
            harness.native_fault(&env);
            assert_eq!(harness.fabric.pending_faults(), 0);
        }
        // One complete rule bag covers Bind/WriteImm/Invalidate x all three faults.
        assert!(harness.native_rules.is_empty());
        assert_eq!(harness.coverage.native_faults.len(), 9);
        assert!(harness.coverage.native_faults.values().all(|&n| n == 1));
        assert!(harness.coverage.native_writes > 0);
        while !harness.nodes.is_empty() {
            harness.remove(&env, 0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

pub(super) fn dst_refused_peer_connect_preserves_candidate_fallback() {
    for native in [false, true] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(119);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(119, sim, clock, native);
        for _ in 0..4 {
            harness.add(&env, None);
        }
        harness.update(2);
        let ranked = harness.ranked_nodes(&harness.object_id(2));
        let requester = (0..4).find(|node| !ranked.contains(node)).unwrap();
        // The first cold request from a noncandidate connects to a peer before
        // origin. Raw ECONNREFUSED must retain the same fallback as synthetic Io.
        harness
            .sim
            .inject("connect", Fault::Errno(libc::ECONNREFUSED))
            .unwrap();
        let client = harness.request_on(2, false, false, requester);
        harness.exchange(client, false);
        assert_eq!(harness.coverage.observed.get("connect"), Some(&1));
        assert_eq!(harness.coverage.success, 1);
        assert_eq!(harness.coverage.failures, 0);
        assert!(harness.coverage.relay_turns > 0);
        while !harness.nodes.is_empty() {
            harness.remove(&env, 0);
        }
        assert_eq!(harness.sim.live_handles(), 0);
        assert_eq!(harness.fabric.live_resources(), 0);
    }
}

pub(super) fn dst_shutdown_checkpoint_uses_async_io_and_releases_all_shards() {
    use crate::store::checkpoint::read_candidates;
    for case in [
        "empty",
        "periodic",
        "write-error",
        "rename-error",
        "budget",
        "populated-budget",
        "overflow",
        "cancel",
        "cancel-write",
    ] {
        let sim = SimOs::new();
        let clock = racer_io::SimClock::new(120);
        let env = racer_io::Env::sim(&sim, clock.ctx(0));
        let mut harness = Harness::new(120, sim, clock, false);
        harness.rng = Random((0..100).find(|seed| Random(*seed).pick(2) == 1).unwrap());
        harness.add(&env, None);
        assert_eq!(harness.nodes[0].workers.len(), 2);
        harness.settle();
        if case == "periodic" {
            harness.clock.advance(Duration::from_secs(6));
            for _ in 0..128 {
                harness.tick();
            }
        }
        let node = &mut harness.nodes[0];
        let state = node.workers[0].app.node.clone();
        let before = read_candidates(&env, &node.config.slab_directory).unwrap();
        let sequence = before.first().map_or(0, |(_, image)| image.sequence);
        assert_eq!(sequence > 0, case == "periodic");
        if case == "overflow" {
            state.periodic_checkpoint.lock().unwrap().last_sequence = u64::MAX;
        }
        if case == "budget" {
            node.workers[0].app.checkpoint_budget = 1;
        }
        if case == "populated-budget" {
            for worker in &mut node.workers {
                worker.app.checkpoint_budget = 32 * 1024;
                let index = worker.app.store.writer.index();
                index.set_page_capacity(1_000_000).unwrap();
                for number in 0u64..128 {
                    let mut key = [0; 32];
                    key[..8].copy_from_slice(&number.to_le_bytes());
                    let version = crate::model::ObjectVersion {
                        object: crate::model::ObjectId {
                            cache: worker.app.caches[0].id.clone(),
                            key: crate::model::CacheKey(key),
                        },
                        etag: crate::model::StrongEtag::parse(b"\"budget\"").unwrap(),
                    };
                    index
                        .publish_version(crate::model::VersionMetadata {
                            version,
                            length: 0,
                            content_type: None,
                        })
                        .unwrap();
                }
                assert!(!index.snapshot_metadata().is_empty());
            }
        }
        let deadline = scope(&env.ctx.clock, Duration::from_secs(30)).unwrap();
        if case == "cancel" {
            deadline.cancel().unwrap();
        }
        match case {
            "write-error" => harness
                .sim
                .inject("write", Fault::Errno(libc::ENOSPC))
                .unwrap(),
            "rename-error" => harness
                .sim
                .inject("rename", Fault::Errno(libc::EIO))
                .unwrap(),
            "empty" | "periodic" | "cancel-write" => {
                harness.sim.inject("write", Fault::Delay(32)).unwrap()
            }
            _ => (),
        }
        harness.sim.take_trace();
        let mut operations: Vec<_> = node
            .workers
            .iter()
            .map(|worker| Some(Box::pin(worker.app.checkpoint(&deadline))))
            .collect();
        let mut results = Vec::new();
        let mut delayed = false;
        let waker = wake::waker();
        let mut cx = Context::from_waker(&waker);
        for turn in 0..256 {
            for worker in &node.workers {
                worker.runtime.reactor.poll_budgeted(64).unwrap();
            }
            for operation in &mut operations {
                if let Some(task) = operation
                    && let Poll::Ready(result) = task.as_mut().poll(&mut cx)
                {
                    results.push(result);
                    *operation = None;
                }
            }
            if turn == 16 && matches!(case, "empty" | "periodic" | "cancel-write") {
                assert!(
                    results.is_empty(),
                    "publication did not yield for delayed I/O"
                );
                for worker in &node.workers {
                    assert!(matches!(
                        futures::executor::block_on(worker.app.store.checkpoint.snapshot_shard()),
                        Err(Error::Overloaded)
                    ));
                }
                delayed = true;
                if case == "cancel-write" {
                    deadline.cancel().unwrap();
                }
            }
            if operations.iter().all(Option::is_none) {
                break;
            }
        }
        assert_eq!(results.len(), node.workers.len(), "{case}");
        let expected = match case {
            "write-error" => Err(Error::Os(libc::ENOSPC)),
            "rename-error" => Err(Error::RenameUncertain(crate::error::PublicationCause::Os(
                libc::EIO,
            ))),
            "budget" | "populated-budget" => Err(Error::Overloaded),
            "overflow" => Err(Error::Unavailable),
            "cancel" | "cancel-write" => Err(Error::Cancelled),
            _ => Ok(()),
        };
        assert!(
            results.iter().all(|result| *result == expected),
            "{case}: {results:?}"
        );
        let after = read_candidates(&env, &node.config.slab_directory).unwrap();
        if expected.is_ok() {
            assert!(delayed);
            assert_eq!(after[0].1.sequence, sequence + 1);
            assert_eq!(after[0].1.shards.len(), node.workers.len());
            assert_eq!(after[0].0, before.first().map_or(0, |(slot, _)| slot ^ 1));
            assert!(
                harness
                    .sim
                    .take_trace()
                    .iter()
                    .any(|event| event.operation == "complete:rename")
            );
        } else {
            assert_eq!(after.len(), before.len(), "{case}");
            if case == "populated-budget" {
                assert!(
                    !harness
                        .sim
                        .take_trace()
                        .iter()
                        .any(|event| event.operation == "submit:write"
                            || event.operation == "submit:rename")
                );
            }
        }
        drop(operations);
        for worker in &node.workers {
            futures::executor::block_on(worker.app.store.checkpoint.snapshot_shard()).unwrap();
            worker.app.store.checkpoint.finish_snapshot();
        }
        // The test directly exercised the final cut. Process-loss cleanup avoids
        // asking a failed shutdown cut to run again, while fencing all I/O owners.
        harness.retire(&env, 0, true);
        assert_eq!(harness.sim.live_handles(), 0);
    }
}

/// Replays every seed once (racer-dst double-runs the whole corpus) and, when `required`,
/// asserts the corpus coverage obligations; returns a digest over all seeds.
fn run_corpus(native: bool, seeds: &[u64], steps: usize, required: bool) -> [u8; 32] {
    let mut digest = Sha256::new();
    let mut counts = BTreeMap::new();
    eprintln!(
        "dst corpus native={native} seeds={seeds:?} steps={steps} coverage-required={required}"
    );
    for &seed in seeds {
        let first = replay(seed, steps, native);
        digest.update(first.trace.digest());
        eprintln!(
            "dst replay seed={seed} native={native} events={} digest={:x}",
            first.trace.events,
            first.trace.hash.clone().finalize()
        );
        first.add_to_corpus(&mut counts, native);
    }
    let missing = coverage_misses(&counts);
    eprintln!(
        "dst corpus coverage native={native} required={required} counts={counts:?} missing={missing:?}"
    );
    assert!(
        !required || missing.is_empty(),
        "DST COVERAGE MISS: default corpus native={native} steps={steps} missing={missing:?}; per-run correctness and exact replay passed"
    );
    digest.finalize().into()
}

#[cfg(test)]
fn requires_corpus_coverage(custom_seeds: Option<&str>, steps: usize) -> bool {
    custom_seeds.is_none() && steps >= WEIGHTED_ACTIONS.len()
}

fn coverage_misses(counts: &BTreeMap<String, usize>) -> Vec<&str> {
    counts
        .iter()
        .filter(|(name, count)| {
            let minimum = match name.as_str() {
                "successful-responses" => 8,
                "response-bytes" => PAGE_BYTES as usize + 1,
                _ => 1,
            };
            **count < minimum
        })
        .map(|(name, _)| name.as_str())
        .collect()
}

#[test]
fn dst_coverage_policy_and_aggregation() {
    let cycle = WEIGHTED_ACTIONS.len();
    assert!(requires_corpus_coverage(None, cycle));
    assert!(requires_corpus_coverage(None, cycle + 1));
    assert!(!requires_corpus_coverage(None, cycle - 1));
    for seeds in ["42", "1,7,42"] {
        for steps in [1, cycle, 512] {
            assert!(!requires_corpus_coverage(Some(seeds), steps));
        }
    }

    // Complementary runs satisfy obligations together, without counting the
    // replay twice. Missing evidence remains visible even in report-only mode.
    let first = Coverage {
        success: 4,
        bytes: PAGE_BYTES as usize,
        actions: BTreeMap::from([("multi-worker", 1)]),
        origin_faults: BTreeMap::from([("credential-reject", 1)]),
        native_faults: BTreeMap::from([("Bind:reject".into(), 1)]),
        ..Coverage::default()
    };
    let second = Coverage {
        success: 4,
        bytes: 1,
        secondary_worker_turns: 1,
        origin_faults: BTreeMap::from([("credential-forbidden", 1)]),
        native_faults: BTreeMap::from([("WriteImm:delay".into(), 1)]),
        ..Coverage::default()
    };
    let mut counts = BTreeMap::new();
    first.add_to_corpus(&mut counts, true);
    let missing = coverage_misses(&counts);
    for name in [
        "successful-responses",
        "response-bytes",
        "secondary-worker-data-turns",
        "origin:credential-forbidden",
        "native:WriteImm:delay",
    ] {
        assert!(missing.contains(&name), "{name}");
    }
    second.add_to_corpus(&mut counts, true);
    let missing = coverage_misses(&counts);
    for name in [
        "successful-responses",
        "response-bytes",
        "action:multi-worker",
        "secondary-worker-data-turns",
        "origin:credential-reject",
        "origin:credential-forbidden",
        "native:Bind:reject",
        "native:WriteImm:delay",
    ] {
        assert!(!missing.contains(&name), "{name}");
    }
    assert!(missing.contains(&"native:Invalidate:completion"));
    assert!(missing.contains(&"action:disk-corruption"));
    assert_eq!(counts["action:multi-worker"], 1);
    // Supplying evidence for every remaining obligation clears the diagnostic.
    for count in counts.values_mut() {
        *count = (*count).max(1);
    }
    assert!(coverage_misses(&counts).is_empty());
    assert!(
        first
            .corpus_counts(false)
            .keys()
            .all(|name| !name.starts_with("native"))
    );
}

fn replay(seed: u64, steps: usize, native: bool) -> Coverage {
    let sim = SimOs::new();
    let clock = racer_io::SimClock::new(seed);
    let env = racer_io::Env::sim(&sim, clock.ctx(0));
    let mut harness = Harness::new(seed, sim, clock, native);
    harness
        .coverage
        .trace
        .record(format!("run:{seed}:{steps}:{native}"));
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        harness.generated(&env, steps)
    }));
    if let Err(error) = result {
        harness.coverage.collect(&harness.sim);
        harness.coverage.collect_native(&harness.fabric);
        eprintln!(
            "DST CORRECTNESS FAILURE seed={seed} steps={steps} native={native} elapsed={:?} trace={:?}",
            harness.clock.elapsed(),
            harness.coverage.trace
        );
        std::panic::resume_unwind(error);
    }
    harness.coverage
}

fn member(config: &Config) -> crate::topology::Member {
    crate::topology::Member {
        node: config.node.clone(),
        shares: std::num::NonZeroU32::new(1).unwrap(),
        peer_endpoint: config.peer_listen.to_string(),
        rails: if config.enable_rdma {
            vec![racer_wire::control::RailMapping {
                rail: racer_wire::control::RailId(0),
                device: format!(
                    "dst-rnic-{}",
                    usize::from_str_radix(&config.node.0[..8], 16).unwrap()
                ),
                port: 1,
                gid: None,
                numa_node: None,
            }]
        } else {
            vec![]
        },
        site: "site1".into(),
    }
}
#[derive(Clone, Copy)]
enum Lifecycle {
    Start,
    Drain,
    Shutdown,
}
fn lifecycle(workers: &mut [LocalWorker], scope: &RequestScope, phase: Lifecycle) {
    let mut pending: Vec<_> = workers
        .iter_mut()
        .map(|worker| {
            let directory = worker.app.directory.clone();
            let coordinator = worker.app.coordinator.clone();
            let id = worker.app.worker;
            let installed = std::cell::Cell::new(!matches!(phase, Lifecycle::Start));
            let operation = match phase {
                Lifecycle::Start => worker.app.start(scope),
                Lifecycle::Drain => worker.app.drain(scope),
                Lifecycle::Shutdown => worker.app.shutdown(scope),
            };
            let mut operation = operation;
            let operation: Operation<'_, ()> = Box::pin(std::future::poll_fn(move |cx| {
                let _local =
                    directory.simulation_scope(installed.get().then(|| (id, coordinator.clone())));
                let result = operation.as_mut().poll(cx);
                // Installation can happen during a Pending startup poll. The next
                // poll selects this worker without attempting another installation.
                if matches!(result, Poll::Ready(_)) {
                    installed.set(true);
                }
                result
            }));
            (&worker.runtime, &mut worker.crypto, Some(operation))
        })
        .collect();
    for _ in 0..MAX_TURNS {
        for (runtime, crypto, operation) in &mut pending {
            runtime.reactor.poll_budgeted(64).unwrap();
            crypto
                .poll_budgeted(&mut Context::from_waker(&wake::waker()), 64)
                .unwrap();
            runtime.crypto.poll_budgeted(64).unwrap();
            if let Some(op) = operation
                && let Poll::Ready(result) =
                    op.as_mut().poll(&mut Context::from_waker(&wake::waker()))
            {
                result.unwrap();
                *operation = None;
            }
        }
        if pending.iter().all(|(_, _, op)| op.is_none()) {
            return;
        }
    }
    panic!("multi-worker lifecycle stalled");
}
fn drive_local<T>(
    runtime: &WorkerRuntime,
    crypto: &mut dyn crate::lifecycle::Service<RequestScope>,
    mut operation: Operation<'_, T>,
) -> Result<T> {
    for _ in 0..MAX_TURNS {
        runtime.reactor.poll_budgeted(64)?;
        crypto.poll_budgeted(&mut Context::from_waker(&wake::waker()), 64)?;
        runtime.crypto.poll_budgeted(64)?;
        if let Poll::Ready(result) = operation
            .as_mut()
            .poll(&mut Context::from_waker(&wake::waker()))
        {
            return result;
        }
    }
    panic!("local application lifecycle stalled")
}
