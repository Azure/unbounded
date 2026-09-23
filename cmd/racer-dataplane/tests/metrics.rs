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
    fn isolated_publication_aggregation_and_idle_deadline() {
        let a = Local::default();
        let b = Local::default();
        assert_eq!(std::ptr::from_ref(&*a.private) as usize % 128, 0);
        assert_eq!(std::ptr::from_ref(&*a.snapshot) as usize % 128, 0);
        let registry = Registry::new(3, Arc::new(crate::control::Updates::default()));
        registry.register(0, &a);
        registry.register(1, &b);
        // Process-wide families (such as TLS offload) are independent of
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
