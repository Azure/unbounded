// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod tests {
    use super::*;

    #[test]
    fn storage_metrics_have_fixed_series_and_do_not_change_readiness() {
        use crate::control::StorageResult;
        let updates = Arc::new(crate::control::Updates::default());
        let registry = Registry::new(0, updates.clone());
        let before = registry.status();
        let series = |text: String| -> std::collections::BTreeSet<String> {
            text.lines()
                .filter(|line| line.starts_with("racer_dataplane_cache_storage_"))
                .map(|line| line.rsplit_once(' ').unwrap().0.to_owned())
                .collect()
        };
        let initial = series(registry.render());
        assert_eq!(initial.len(), 8);
        updates.observe_storage(1 << 30, 3);
        for version in 1..=20 {
            updates.test_storage_policy(version, 2 << 30);
            let request = updates.desired_storage().unwrap();
            assert!(updates.report_storage(
                &request,
                StorageResult::Failed(format!("disk-{version}\nfull")),
                1 << 30
            ));
            let text = registry.render();
            assert_eq!(series(text.clone()), initial);
            assert!(!text.contains("disk-") && !text.contains("policyIdentity"));
            assert!(text.contains("racer_dataplane_cache_storage_applied_bytes 1073741824\n"));
            assert!(text.contains("racer_dataplane_cache_storage_effective_bytes 2147483648\n"));
            assert!(text.contains("racer_dataplane_cache_storage_shards 3\n"));
            assert!(text.contains("racer_dataplane_cache_storage_phase{phase=\"failed\"} 1\n"));
            assert_eq!(registry.status()["ready"], before["ready"]);
            assert_eq!(registry.status()["lastError"], before["lastError"]);
        }
        let request = updates.desired_storage().unwrap();
        assert!(updates.report_storage(&request, StorageResult::Applied, 2 << 30));
        assert!(
            registry
                .render()
                .contains("racer_dataplane_cache_storage_phase{phase=\"applied\"} 1\n")
        );
        assert_eq!(series(registry.render()), initial);
    }

    #[test]
    fn allocator_diagnostics_are_bounded_summed_and_removed_without_losing_counters() {
        let registry = Registry::new(2, Arc::new(crate::control::Updates::default()));
        let locals = [Local::default(), Local::default()];
        let mut state = [0; 11];
        state[2] = 1;
        state[6..].copy_from_slice(&[2, 3, 9, 4096, 1]);
        for (worker, local) in locals.iter().enumerate() {
            registry.register(worker, local);
            local.allocator_counters([1, 2, 3, 4, 3]);
            local.allocator_state([0; 11], state);
        }
        assert!(
            registry
                .render()
                .contains("racer_dataplane_allocator_checkpoints_total{event=\"completed\"} 0\n")
        );
        for local in &locals {
            local.publish();
        }
        let text = registry.render();
        let samples: Vec<_> = text
            .lines()
            .filter(|line| line.starts_with("racer_dataplane_allocator_"))
            .collect();
        assert_eq!(samples.len(), 16);
        assert_eq!(
            samples
                .iter()
                .map(|line| line.rsplit_once(' ').unwrap().0)
                .collect::<std::collections::BTreeSet<_>>()
                .len(),
            16
        );
        for (sample, count) in [
            ("payload_rejections_total{reason=\"pending_limit\"}", 2),
            (
                "payload_rejections_total{reason=\"filesystem_headroom\"}",
                4,
            ),
            ("payload_rejections_total{reason=\"extent_unavailable\"}", 6),
            ("checkpoints_total{event=\"prepared\"}", 8),
            ("checkpoints_total{event=\"completed\"}", 6),
            ("checkpoint_shards{phase=\"data_sync\"}", 2),
            ("pressure{resource=\"charged_bytes\"}", 8192),
            ("pressure{resource=\"reclaim_shards\"}", 2),
        ] {
            assert!(text.contains(&format!("racer_dataplane_allocator_{sample} {count}\n")));
        }
        locals[0].allocator_state(state, [0; 11]);
        locals[0].publish();
        assert!(
            registry
                .render()
                .contains("racer_dataplane_allocator_pressure{resource=\"charged_bytes\"} 4096\n")
        );
        locals[1].allocator_state(state, [0; 11]);
        locals[1].publish();
        let text = registry.render();
        assert!(
            text.contains("racer_dataplane_allocator_pressure{resource=\"charged_bytes\"} 0\n")
        );
        assert!(
            text.contains("racer_dataplane_allocator_checkpoints_total{event=\"completed\"} 6\n")
        );
        assert_eq!(
            text.lines()
                .filter(|line| line.starts_with("racer_dataplane_http_"))
                .count(),
            76
        );
    }

    #[test]
    fn disk_cache_evictions_aggregate_only_after_publication() {
        let registry = Registry::new(2, Arc::new(crate::control::Updates::default()));
        let locals = [Local::default(), Local::default()];
        let sample = "racer_dataplane_disk_cache_evictions_total";
        for (worker, local) in locals.iter().enumerate() {
            registry.register(worker, local);
            local.disk_cache_evictions(0);
            assert!(!local.private.dirty.get());
            local.disk_cache_evictions(worker as u64 + 2);
        }
        assert!(registry.render().contains(&format!("{sample} 0\n")));
        for local in &locals {
            local.publish();
        }
        let text = registry.render();
        assert!(text.contains(&format!("# TYPE {sample} counter\n")));
        assert!(text.contains(&format!("{sample} 5\n")));
        assert_eq!(
            text.lines().filter(|line| line.starts_with(sample)).count(),
            1
        );
        locals[0].disk_cache_evictions(4);
        locals[0].publish();
        assert!(registry.render().contains(&format!("{sample} 9\n")));
    }

    #[test]
    fn resource_exhaustion_sites_are_bounded_and_published_across_workers() {
        let sites = [
            (ResourceWaitSite::NetworkFlight, "network_flight"),
            (ResourceWaitSite::MetadataAdmission, "metadata_admission"),
            (ResourceWaitSite::UpstreamAdmission, "upstream_admission"),
            (ResourceWaitSite::PayloadAdmission, "payload_admission"),
            (ResourceWaitSite::MaterializeRead, "materialize_read"),
            (ResourceWaitSite::MaterializeBuffer, "materialize_buffer"),
            (ResourceWaitSite::ChecksumQueue, "checksum_queue"),
            (ResourceWaitSite::ReceiveBuffer, "receive_buffer"),
        ];
        let registry = Registry::new(2, Arc::new(crate::control::Updates::default()));
        let locals = [Local::default(), Local::default()];
        for (worker, local) in locals.iter().enumerate() {
            registry.register(worker, local);
            for (index, (site, _)) in sites.iter().enumerate() {
                for _ in 0..index + worker + 1 {
                    local.resource_exhaustion(*site);
                }
            }
        }
        for published in [false, true] {
            if published {
                for local in &locals {
                    local.publish();
                }
            }
            let text = registry.render();
            let samples: Vec<_> = text
                .lines()
                .filter(|line| {
                    line.starts_with("racer_dataplane_cache_resource_exhaustions_total{")
                })
                .collect();
            assert_eq!(samples.len(), 8);
            let unique: std::collections::BTreeSet<_> = samples.iter().copied().collect();
            assert_eq!(unique.len(), 8);
            for (index, (_, label)) in sites.iter().enumerate() {
                let expected = if published { 2 * index + 3 } else { 0 };
                assert!(unique.contains(format!("racer_dataplane_cache_resource_exhaustions_total{{site=\"{label}\"}} {expected}").as_str()));
            }
            assert_eq!(
                text.lines()
                    .filter(|line| line.starts_with("racer_dataplane_http_"))
                    .count(),
                76
            );
        }
    }

    #[test]
    fn http_failure_series_are_bounded_and_aggregate_after_publication() {
        let registry = Registry::new(2, Arc::new(crate::control::Updates::default()));
        let locals = [Local::default(), Local::default()];
        let failure = HttpFailure {
            reason: HttpErrorReason::Busy,
            pressure: Some(HttpPressure::Admission),
        };
        for (worker, local) in locals.iter().enumerate() {
            registry.register(worker, local);
            local.http_failure(false, false, failure);
            local.http_failure(true, true, failure);
        }
        let error = "racer_dataplane_http_error_responses_total{source=\"client\",status=\"503\",reason=\"busy\"}";
        assert!(registry.render().contains(&format!("{error} 0\n")));
        for local in &locals {
            local.publish();
        }
        let text = registry.render();
        assert!(text.contains(&format!("{error} 2\n")));
        assert!(text.contains(
            "racer_dataplane_http_stream_aborts_total{source=\"peer\",reason=\"busy\"} 2\n"
        ));
        assert!(text.contains("racer_dataplane_http_pressure_failures_total{source=\"client\",event=\"error_response\",cause=\"admission\"} 2\n"));
        let samples: Vec<_> = text
            .lines()
            .filter(|s| s.starts_with("racer_dataplane_http_"))
            .collect();
        assert_eq!(samples.len(), 76);
        assert!(
            samples
                .iter()
                .all(|s| !s.contains("target=") && !s.contains("peer="))
        );
        let unique: std::collections::BTreeSet<_> = samples
            .iter()
            .map(|s| s.rsplit_once(' ').unwrap().0)
            .collect();
        assert_eq!(unique.len(), samples.len());
    }

    #[test]
    fn peer_snapshots_are_isolated_replaced_and_escaped() {
        use crate::breaker::Status;
        let registry = Registry::new(2, Arc::new(crate::control::Updates::default()));
        let locals = [Local::default(), Local::default()];
        for (worker, local) in locals.iter().enumerate() {
            registry.register(worker, local);
            local.publish_peers(vec![PeerState {
                volume: "v\"\\\n1".into(),
                peer: "p1".into(),
                http: Status::Closed,
                rdma: if worker == 0 {
                    Status::Open
                } else {
                    Status::HalfOpen
                },
                prefer_rdma: worker == 0,
            }]);
        }
        let text = registry.render();
        let mut series = std::collections::BTreeSet::new();
        let samples: Vec<_> = text
            .lines()
            .filter(|line| line.starts_with("racer_dataplane_peer_"))
            .collect();
        assert_eq!(samples.len(), 16);
        let transport_start = text.find("# HELP racer_dataplane_peer_transport").unwrap();
        assert!(!text[transport_start..].contains("racer_dataplane_peer_circuit_breaker_state"));
        for sample in samples {
            let (name, value) = sample.rsplit_once(' ').unwrap();
            assert!(series.insert(name));
            assert!(value == "0" || value == "1");
            assert!(name.contains(r#"volume="v\"\\\n1""#));
        }
        assert!(text.contains(r#"peer="p1",transport="rdma",state="half_open"} 1"#));
        assert!(text.contains(r#"peer="p1",transport="rdma",state="open"} 1"#));
        // A busy exporter cannot block a worker's publication.
        let held = locals[0].snapshot.1.lock().unwrap();
        locals[0].publish_peers(Vec::new());
        drop(held);
        assert_eq!(registry.render(), text);
        locals[0].publish_peers(Vec::new());
        let text = registry.render();
        assert!(!text.contains("worker=\"0\""));
        assert!(text.contains("worker=\"1\""));
        locals[1].publish_peers(Vec::new());
        assert!(!registry.render().contains("peer=\"p1\""));
    }

    #[test]
    fn rdma_dense_free_index_corpus() {
        use crate::{buffers, rdma};
        use std::panic::{AssertUnwindSafe, catch_unwind};

        let pool = buffers::io_test_pool(1);
        for depth in [1, 16, 17, 64] {
            let transport = rdma::test_transport_config(&pool, 1, depth);
            transport.test_progress(32).unwrap();
            let count = depth * 4;
            let before = transport.test_invariants();
            assert_eq!(before, (count, count, 0));
            let forward: Vec<_> = (0..count).collect();
            let reverse: Vec<_> = forward.iter().copied().rev().collect();
            transport.test_free_indices(&forward);
            transport.test_free_indices(&reverse);
            transport.test_free_indices(&[]);

            // Duplicates at both ends and across u64 words, plus indexes in
            // the padding of a partial final word and wholly foreign words.
            for bad in [
                vec![0, 0],
                vec![count - 1, count - 1],
                vec![count],
                vec![count + 63],
                vec![usize::MAX],
            ] {
                assert!(
                    catch_unwind(AssertUnwindSafe(|| transport.test_free_indices(&bad))).is_err()
                );
                assert_eq!(transport.test_invariants(), before);
            }
            if count > 64 {
                transport.test_free_indices(&[0, 63, 64, count - 1]);
                assert!(
                    catch_unwind(AssertUnwindSafe(|| {
                        transport.test_free_indices(&[0, 63, 64, 63]);
                    }))
                    .is_err()
                );
            }
            // Exercise the actual production free list with posted receive WRs.
            let pending = transport.prepare([7; 16], 0, 1).unwrap();
            assert_eq!(transport.test_invariants(), (count, count - depth * 2, 0));
            assert!(
                catch_unwind(AssertUnwindSafe(|| transport.test_free_indices(&forward))).is_err()
            );
            drop(pending);
            transport.test_progress(32).unwrap();
            assert_eq!(transport.test_invariants(), before);
        }
    }

    #[test]
    fn rdma_counts_only_valid_ack_once_even_before_grant_send_retirement() {
        use crate::{buffers, crypto, rdma};
        for invalid in [false, true] {
            let ap = buffers::io_test_pool(1);
            let bp = buffers::io_test_pool(1);
            let a = rdma::test_transport_config(&ap, 1, 1);
            let b = rdma::test_transport_config(&bp, 1, 1);
            let metrics = Local::default();
            b.set_metrics(metrics.clone()).unwrap();
            let aq = a.prepare([7; 16], 0, 1).unwrap();
            let bq = b.prepare([7; 16], 0, 1).unwrap();
            let policy = crypto::tests::trust(7).1;
            let peers = crypto::auth::PeerContext::new([1; 32], [2; 32]).unwrap();
            let (i, hello) = crypto::auth::Initiator::start(
                policy.clone(),
                peers.clone(),
                Some(aq.offer()),
                Duration::from_secs(30),
            )
            .unwrap();
            let (r, reply) = crypto::auth::Responder::accept(
                policy.clone(),
                peers,
                hello,
                Some(bq.offer()),
                Duration::from_secs(30),
            )
            .unwrap();
            let (sa, finish) = i.finish(reply).unwrap();
            let ac = aq.connect_authenticated(sa, policy.clone(), 0).unwrap();
            let bc = bq
                .connect_authenticated(r.finish(finish).unwrap(), policy, 0)
                .unwrap();
            let fill = |pool: &buffers::WorkerPool| pool.stage(buffers::Key::new([9; 32])).unwrap();
            let mut request = ac.request([9; 32], 17, b"metrics").unwrap();
            ac.test_pump(&bc, false);
            let mut source = fill(&bp);
            source.as_mut_slice()[..17].fill(42);
            let crc = crate::allocator::crc64(&source.as_mut_slice()[..17]);
            bc.respond(
                bc.next_request().unwrap().unwrap(),
                source.publish_checked(17, crc).unwrap(),
            )
            .unwrap();
            bc.test_pump(&ac, false); // BIND retires, grant SEND remains outstanding.
            let (sender, receiver) = (bc.test_endpoint().1, ac.test_endpoint().1);
            let grant_send = sender.posts()[0];
            assert!(sender.effect(&receiver, grant_send, false).unwrap());
            assert!(receiver.complete(receiver.receives()[0], 0).unwrap());
            a.test_progress(32).unwrap();
            assert_eq!(metrics.values()[5], 0, "advertisement is not payload");
            let grant = ac.take_grant(&mut request).unwrap().unwrap();
            let read = ac.read(grant, fill(&ap)).unwrap();
            if invalid {
                a.test_edit_control(|body| body[67] ^= 1);
            }
            ac.test_pump(&bc, false); // READ retires and signs ACK.
            let ack = a.test_observe().sends[0].1.clone();
            b.test_inject(&ack).unwrap();
            let expected = if invalid { 0 } else { 17 };
            assert_eq!(metrics.values()[5], expected, "only an exact ACK counts");
            if !invalid {
                assert_eq!(b.test_observe().sends[0].0, grant_send.id);
                assert_eq!(b.test_invariants().2, 1, "early ACK retains source");
                b.test_inject(&ack).unwrap(); // Authenticated replay retires the QP.
                assert_eq!(metrics.values()[5], expected, "replay cannot double count");
            }
            assert!(!bc.is_healthy());
            drop(read);
            a.shutdown().unwrap();
            b.shutdown().unwrap();
        }
    }

    #[test]
    fn isolated_publication_aggregation_and_idle_deadline() {
        let a = Local::default();
        let b = Local::default();
        assert_eq!(std::ptr::from_ref(&*a.private) as usize % 128, 0);
        assert_eq!(std::ptr::from_ref(&*a.snapshot) as usize % 128, 0);
        let registry = Registry::new(3, Arc::new(crate::control::Updates::default()));
        registry.register(0, &a);
        registry.register(1, &b);
        // Process-wide families (such as replay admission) are independent of
        // worker counters. Check series identity/cardinality, not a total tied
        // to the private counter array's length.
        let series = |text: String| {
            let mut names = std::collections::BTreeSet::new();
            for sample in text.lines().filter(|s| !s.starts_with('#')) {
                let (name, value) = sample.rsplit_once(' ').unwrap();
                value.parse::<u64>().unwrap();
                assert!(names.insert(name.to_owned()), "duplicate series: {name}");
            }
            names
        };
        let shape = series(registry.render());
        assert_eq!(
            shape,
            series(Registry::new(0, Arc::new(crate::control::Updates::default())).render()),
            "series must not depend on worker registration"
        );
        for (source, transport) in TRAFFIC {
            for family in ["requests_total", "response_bytes_total"] {
                assert!(shape.contains(&format!(
                    "racer_dataplane_{family}{{source=\"{source}\",transport=\"{transport}\"}}"
                )));
            }
        }
        assert!(shape.contains("racer_dataplane_config_epoch"));
        assert!(shape.contains("racer_dataplane_storage_quarantines_total"));
        assert!(shape.contains(
            "racer_dataplane_cache_lookups_total{kind=\"metadata\",result=\"memory_hit\"}"
        ));
        assert!(
            !shape.contains(
                "racer_dataplane_cache_lookups_total{kind=\"page\",result=\"memory_hit\"}"
            )
        );
        assert!(!shape.contains(
            "racer_dataplane_cache_lookups_total{kind=\"metadata\",result=\"disk_hit\"}"
        ));
        let line = "racer_dataplane_requests_total{source=\"client\",transport=\"http\"}";
        let mut deadline = None;
        assert!(a.poll(&mut deadline).deadline.is_none());
        a.request(Traffic::ClientHttp);
        b.request(Traffic::ClientHttp);
        assert!(registry.render().contains(&format!("{line} 0\n")));
        let end = a.poll(&mut deadline).deadline.unwrap();
        assert_eq!(a.poll(&mut deadline).deadline, Some(end));
        // A worker can become idle after its last event. The scheduled wakeup
        // publishes without requiring another request or a scrape handshake.
        deadline = Some(crate::environment::now());
        assert!(a.poll(&mut deadline).deadline.is_none());
        assert!(registry.render().contains(&format!("{line} 1\n")));
        b.publish();
        drop(b);
        assert!(registry.render().contains(&format!("{line} 2\n")));
        assert!(a.poll(&mut deadline).deadline.is_none());
        assert_eq!(shape, series(registry.render()));
    }

    #[test]
    fn exporter_serves_only_metrics_and_bounds_slow_clients() {
        let registry = Arc::new(Registry::new(
            1,
            Arc::new(crate::control::Updates::default()),
        ));
        let local = Local::default();
        registry.register(0, &local);
        local.request(Traffic::PeerRdma);
        local.publish();
        local.publish_peers(vec![PeerState {
            volume: "v1".into(),
            peer: "p1".into(),
            http: crate::breaker::Status::Closed,
            rdma: crate::breaker::Status::Open,
            prefer_rdma: false,
        }]);
        let exporter = Exporter::start("127.0.0.1:0".parse().unwrap(), registry.clone()).unwrap();
        assert!(Exporter::start(exporter.address(), registry.clone()).is_err());
        let scrape = |path: &str| {
            let mut socket = TcpStream::connect(exporter.address()).unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            write!(socket, "GET {path} HTTP/1.1\r\nHost: localhost\r\n\r\n").unwrap();
            let mut response = String::new();
            socket.read_to_string(&mut response).unwrap();
            response
        };
        assert!(scrape("/metrics").ends_with(&registry.render()));
        let response = scrape("/status");
        assert!(response.starts_with("HTTP/1.1 200"));
        let status: serde_json::Value =
            serde_json::from_str(response.split_once("\r\n\r\n").unwrap().1).unwrap();
        assert_eq!(status["storage"]["phase"], "unmanaged");
        assert_eq!(status["storage"]["appliedVersion"], 0);
        assert!(scrape("/anything").starts_with("HTTP/1.1 404"));
        assert_eq!(local.values()[2], 1, "scraping cannot increment traffic");
        let mut slow = Vec::new();
        for _ in 0..32 {
            slow.push(TcpStream::connect(exporter.address()).unwrap());
        }
        // Drop must wake the exporter even with a full slow-client table.
        let start = Instant::now();
        drop(exporter);
        assert!(start.elapsed() < Duration::from_secs(1));

        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (socket, _) = listener.accept().unwrap();
        socket.set_nonblocking(true).unwrap();
        let mut client = Client {
            socket,
            input: vec![b'a'; 8192],
            output: None,
            sent: 0,
            end: Instant::now() + Duration::from_secs(2),
        };
        peer.write_all(b"a").unwrap();
        assert!(!client.poll(&registry).unwrap());
        client.input.clear();
        client.end = Instant::now();
        assert!(!client.poll(&registry).unwrap());
    }
}

#[cfg(test)]
/// Wall-clock instrumentation and bounded independent replay orchestration.
/// Neither helper changes the virtual clock or consumes simulator randomness.
pub(crate) mod dst {
    use std::{
        cell::RefCell,
        collections::BTreeSet,
        time::{Duration, Instant},
    };

    const LABELS: [&str; 9] = [
        "backend+rdma",
        "driver",
        "invariants",
        "observe",
        "schedule",
        "burst",
        "cold",
        "relay",
        "fullmatrix",
    ];
    #[derive(Clone, Copy, Default)]
    struct Total {
        elapsed: Duration,
        calls: u64,
    }
    pub struct Profile {
        enabled: bool,
        totals: RefCell<[Total; 9]>,
    }
    impl Default for Profile {
        fn default() -> Self {
            Self {
                enabled: std::env::var("RACER_DST_PROFILE").is_ok_and(|s| s == "1"),
                totals: RefCell::new([Total::default(); 9]),
            }
        }
    }
    impl Profile {
        #[inline]
        pub fn start(&self) -> Option<Instant> {
            self.enabled.then(Instant::now)
        }
        #[inline]
        pub fn stop(&self, component: usize, began: Option<Instant>) {
            if let Some(began) = began {
                let elapsed = began.elapsed();
                let mut totals = self.totals.borrow_mut();
                totals[component].elapsed += elapsed;
                totals[component].calls += 1;
            }
        }
        pub fn report(&self, nodes: usize, turns: usize) {
            if !self.enabled {
                return;
            }
            eprintln!(
                "DST wall: turn components are exclusive; workload phases include those components; driver includes application origin/cache work"
            );
            for (label, total) in LABELS.iter().zip(self.totals.borrow().iter()) {
                eprintln!(
                    "DST wall nodes={nodes} turns={turns} component={label} calls={} total_ms={:.3}",
                    total.calls,
                    total.elapsed.as_secs_f64() * 1000.0
                );
            }
        }
    }
    pub fn scale_seed(default: u64) -> u64 {
        let seed = std::env::var("RACER_DST_SEED")
            .map_or(default, |s| s.parse().expect("RACER_DST_SEED must be u64"));
        eprintln!("DST scale seed={seed}");
        seed
    }
    pub fn prefix_workers() -> usize {
        let available = std::thread::available_parallelism().map_or(1, usize::from);
        let workers = std::env::var("RACER_DST_PREFIX_WORKERS").map_or(available.min(8), |s| {
            s.parse().expect("RACER_DST_PREFIX_WORKERS must be 1..=8")
        });
        assert!((1..=8).contains(&workers));
        workers
    }
    pub fn explore<T: std::fmt::Debug + PartialEq + Send>(
        setup: Vec<usize>,
        depth: usize,
        workers: usize,
        run: impl Fn(&[usize]) -> (T, Vec<crate::simulation::Choice>) + Sync,
    ) -> usize {
        assert!((1..=8).contains(&workers));
        let began = std::env::var("RACER_DST_PROFILE")
            .is_ok_and(|s| s == "1")
            .then(Instant::now);
        eprintln!("DST prefix workers={workers} additional_depth={depth}");
        let mut frontier = vec![setup.clone()];
        let mut visited = BTreeSet::new();
        while !frontier.is_empty() {
            let mut next = Vec::new();
            // Only a bounded batch of complete worlds/results is resident.
            for batch in frontier.chunks(workers) {
                let results = std::thread::scope(|scope| {
                    let run = &run;
                    let handles: Vec<_> = batch
                        .iter()
                        .map(|prefix| {
                            scope.spawn(move || {
                                assert!(
                                    crate::simulation::current().is_none(),
                                    "worker inherited a World"
                                );
                                let (result, choices) = run(prefix);
                                assert_eq!(result, run(prefix).0, "prefix={prefix:?}");
                                assert!(
                                    crate::simulation::current().is_none(),
                                    "branch leaked its World scope"
                                );
                                choices
                            })
                        })
                        .collect();
                    handles.into_iter().map(|h| h.join()).collect::<Vec<_>>()
                });
                for (prefix, result) in batch.iter().zip(results) {
                    let choices = result.unwrap_or_else(|panic| std::panic::resume_unwind(panic));
                    assert!(visited.insert(prefix.clone()));
                    assert!(choices.len() > prefix.len());
                    if prefix.len() < setup.len() + depth {
                        let choice = choices[prefix.len()];
                        assert_eq!(choice.index as usize, prefix.len());
                        for selected in 0..choice.enabled {
                            let mut child = prefix.clone();
                            child.push(selected);
                            next.push(child);
                        }
                    }
                }
            }
            frontier = next;
        }
        if let Some(began) = began {
            eprintln!(
                "DST prefix visited={} workers={workers} wall_ms={:.3}",
                visited.len(),
                began.elapsed().as_secs_f64() * 1000.0
            );
        }
        visited.len()
    }
    #[test]
    fn parallel_frontiers_keep_worlds_local_and_results_ordered() {
        fn run(workers: usize) -> std::collections::BTreeMap<Vec<usize>, [u8; 32]> {
            let results = std::sync::Mutex::new(std::collections::BTreeMap::new());
            let visited = explore(Vec::new(), 2, workers, |prefix| {
                assert!(crate::simulation::current().is_none());
                let world = crate::simulation::World::new(17);
                let _scope = world.enter();
                world.enable_scheduler();
                world.script(prefix.to_vec());
                let a = world.choose_enabled("a", &[10, 20]);
                let b = world.choose_enabled("b", if a == 0 { &[30, 40] } else { &[30, 40, 50] });
                world.choose_enabled("end", &[60, 70]);
                world.assert_replay_consumed();
                if let Some(previous) = results
                    .lock()
                    .unwrap()
                    .insert(prefix.to_vec(), world.digest())
                {
                    assert_eq!(previous, world.digest());
                }
                ((a, b, world.digest()), world.choices())
            });
            assert_eq!(visited, 8);
            results.into_inner().unwrap()
        }
        assert_eq!(run(1), run(4));
    }
    #[test]
    fn wall_profile_does_not_change_virtual_replay() {
        let run = |enabled| {
            let world = crate::simulation::World::new(23);
            let _scope = world.enter();
            world.enable_scheduler();
            let profile = Profile {
                enabled,
                totals: RefCell::new([Total::default(); 9]),
            };
            for _ in 0..4 {
                let began = profile.start();
                world.service_tick();
                world.choose_enabled("profile-neutrality", &[2, 7, 11]);
                profile.stop(0, began);
            }
            assert_eq!(
                profile.totals.borrow()[0].calls,
                if enabled { 4 } else { 0 }
            );
            (world.tick(), world.digest(), world.choices())
        };
        assert_eq!(run(false), run(true));
    }
}
