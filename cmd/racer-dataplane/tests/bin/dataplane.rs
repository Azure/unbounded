// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod buffer_configuration_tests {
    use super::*;
    use std::process::Command;

    #[test]
    fn daemon_minimum_accepts_four_and_rejects_smaller_positive_pools() {
        for count in 1..4 {
            let error = daemon_pool_config(NonZeroUsize::new(count).unwrap())
                .err()
                .unwrap();
            assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
            assert!(
                error
                    .to_string()
                    .contains("RACER_BUFFERS_PER_NODE must be at least 4")
            );
        }
        for count in [4, 8, 32] {
            let config = daemon_pool_config(NonZeroUsize::new(count).unwrap()).unwrap();
            assert_eq!(config.buffers_per_node.get(), count);
            assert_eq!(config.network_flights.get(), 128);
            assert_eq!(config.consumers_per_flight.get(), 64);
        }
    }

    #[test]
    fn invalid_buffer_configuration_fails_before_bootstrap_and_storage() {
        for value in ["0", "1", "2", "3", "-1", "invalid", ""] {
            let output = Command::new(env::current_exe().unwrap())
                .args([
                    "--exact",
                    "buffer_configuration_tests::invalid_buffer_configuration_child",
                    "--ignored",
                    "--nocapture",
                ])
                .env("RACER_BUFFER_CONFIG_CHILD", "1")
                .env("RACER_BUFFERS_PER_NODE", value)
                .env_remove("RACER_CONTROL_PLANE_URL")
                .env_remove("RACER_FLIGHT_CONSUMERS")
                .output()
                .unwrap();
            assert!(
                output.status.success(),
                "{value:?}: {}",
                String::from_utf8_lossy(&output.stderr)
            );
            assert!(String::from_utf8_lossy(&output.stdout).contains("running 1 test"));
        }
    }

    #[test]
    #[ignore = "subprocess helper isolates environment configuration"]
    fn invalid_buffer_configuration_child() {
        if env::var_os("RACER_BUFFER_CONFIG_CHILD").is_none() {
            return;
        }
        let life = Arc::new(lifecycle::Lifecycle::new(lifecycle::Config::default()));
        let stop = workers::StopHandle::supervised(life.clone());
        let error = run(life, stop).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        assert!(error.to_string().contains("RACER_BUFFERS_PER_NODE"));
    }
}

#[cfg(test)]
mod lifecycle_process_tests {
    use super::*;
    use std::{
        io::{Read, Write},
        net::{TcpListener, TcpStream},
        process::{Command, Stdio},
        thread,
        time::{Duration, Instant},
    };

    struct Never;
    struct Wake;
    impl workers::Wake for Wake {
        fn wake(&self) {}
    }
    impl workers::Driver for Never {
        type Wake = Wake;
        fn wake_handle(&self) -> Arc<Wake> {
            Arc::new(Wake)
        }
        fn turn(&mut self) -> io::Result<()> {
            unreachable!()
        }
        fn shutdown(&mut self) -> io::Result<()> {
            unreachable!()
        }
    }

    #[test]
    #[ignore = "process helper: intentionally blocks a worker factory forever"]
    fn blocked_factory_child() {
        let Ok(address) = env::var("RACER_LIFECYCLE_CHILD") else {
            return;
        };
        install_signals().unwrap();
        let life = Arc::new(lifecycle::Lifecycle::new(lifecycle::Config {
            startup: Duration::from_secs(1),
            quiesce: Duration::from_secs(1),
            ..Default::default()
        }));
        let stop = workers::StopHandle::supervised(life.clone());
        let _monitor = lifecycle::Monitor::start(life.clone(), stop.clone(), &STOP).unwrap();
        let plan = workers::CpuPlan::discover(
            workers::Config {
                shard_count: NonZeroUsize::new(1).unwrap(),
            },
            workers::WorkerCounts {
                io_per_node: NonZeroUsize::new(1),
                compute_per_node: NonZeroUsize::new(1),
            },
        )
        .unwrap();
        life.configure_workers(plan.io().len());
        let _ = workers::Workers::start_supervised::<Never, _>(plan, stop, move |_| {
            let mut channel = TcpStream::connect(&address).unwrap();
            channel.write_all(b"factory entered").unwrap();
            loop {
                thread::park();
            }
        });
        panic!("blocked factory returned");
    }

    #[test]
    fn init_sigterm_and_startup_deadline_terminate_a_blocked_factory_process() {
        for signal in [true, false] {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            let child = Command::new(env::current_exe().unwrap())
                .args([
                    "--exact",
                    "lifecycle_process_tests::blocked_factory_child",
                    "--ignored",
                    "--nocapture",
                    "--test-threads=2",
                ])
                .env(
                    "RACER_LIFECYCLE_CHILD",
                    listener.local_addr().unwrap().to_string(),
                )
                .stdout(Stdio::inherit())
                .stderr(Stdio::inherit())
                .spawn()
                .unwrap();
            struct Guard(std::process::Child);
            impl Drop for Guard {
                fn drop(&mut self) {
                    let _ = self.0.kill();
                    let _ = self.0.wait();
                }
            }
            let mut child = Guard(child);
            let end = Instant::now() + Duration::from_secs(5);
            let mut channel = loop {
                if let Ok((s, _)) = listener.accept() {
                    break s;
                }
                assert!(Instant::now() < end && child.0.try_wait().unwrap().is_none());
                thread::sleep(Duration::from_millis(5));
            };
            channel
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            let mut entered = [0; 15];
            channel.read_exact(&mut entered).unwrap();
            assert_eq!(&entered, b"factory entered");
            let start = Instant::now();
            if signal {
                // SAFETY: signal only the child owned by this test.
                assert_eq!(unsafe { libc::kill(child.0.id() as i32, libc::SIGTERM) }, 0);
            }
            let status = loop {
                if let Some(status) = child.0.try_wait().unwrap() {
                    break status;
                }
                assert!(Instant::now() < end, "hard deadline failed");
                thread::sleep(Duration::from_millis(5));
            };
            assert_eq!(status.code(), Some(124));
            assert!(start.elapsed() < Duration::from_secs(if signal { 2 } else { 3 }));
        }
    }
}

#[cfg(test)]
mod management_address_tests {
    use super::*;
    use std::{
        io::{Read, Write},
        net::TcpStream,
        os::unix::ffi::OsStringExt,
    };

    fn configured(metrics: Option<&str>, pod_ip: Option<&str>) -> io::Result<SocketAddr> {
        management_address(|name| {
            match name {
                "RACER_METRICS_ADDR" => metrics,
                "RACER_POD_IP" => pod_ip,
                _ => panic!("unexpected setting: {name}"),
            }
            .map(str::to_owned)
            .ok_or(env::VarError::NotPresent)
        })
    }

    #[test]
    fn defaults_follow_primary_pod_ip_and_preserve_standalone_binding() {
        for (pod_ip, expected) in [
            (None, "0.0.0.0:9090"),
            (Some("10.20.30.40"), "10.20.30.40:9090"),
            (Some("fd00:1234::42"), "[fd00:1234::42]:9090"),
        ] {
            assert_eq!(configured(None, pod_ip).unwrap(), expected.parse().unwrap());
        }
    }

    #[test]
    fn explicit_metrics_address_is_authoritative() {
        for address in ["127.0.0.1:10000", "0.0.0.0:0", "[::1]:10001", "[::]:9090"] {
            let selected = management_address(|name| {
                assert_eq!(name, "RACER_METRICS_ADDR", "must not read Pod IP override");
                Ok(address.into())
            })
            .unwrap();
            assert_eq!(selected, address.parse().unwrap());
        }
    }

    #[test]
    fn peer_binding_follows_pod_ip_independently_of_management_override() {
        for (metrics, pod, expected_peer) in [
            ("127.0.0.1:9090", Some("127.0.0.2"), "127.0.0.2:9443"),
            ("127.0.0.1:9090", Some("fd00::42"), "[fd00::42]:9443"),
            ("[::1]:9090", Some("10.20.30.40"), "10.20.30.40:9443"),
            ("127.0.0.1:9090", None, "0.0.0.0:9443"),
        ] {
            let lookup = |name: &str| match name {
                "RACER_METRICS_ADDR" => Ok(metrics.to_owned()),
                "RACER_POD_IP" => pod.map(str::to_owned).ok_or(env::VarError::NotPresent),
                _ => panic!("unexpected setting {name}"),
            };
            assert_eq!(
                management_address(lookup).unwrap(),
                metrics.parse().unwrap()
            );
            assert_eq!(
                peer_address(lookup).unwrap(),
                expected_peer.parse().unwrap()
            );
        }
        // A management override cannot conceal an invalid advertised Pod address.
        for bad in ["", "localhost", "127.0.0.2:9443", "[::1]"] {
            let error = peer_address(|name| {
                assert_eq!(name, "RACER_POD_IP");
                Ok(bad.into())
            })
            .unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        }
        let error = peer_address(|_| {
            Err(env::VarError::NotUnicode(std::ffi::OsString::from_vec(
                vec![0xff],
            )))
        })
        .unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
    }

    #[test]
    fn invalid_selected_configuration_fails_instead_of_falling_back() {
        for bad in ["", "localhost", "[::1]", "127.0.0.1:9090", "not-an-ip"] {
            let error = configured(None, Some(bad)).unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
            assert!(error.to_string().contains("RACER_POD_IP"));
        }
        for bad in ["", "localhost:9090", "::1:9090", "[::1]:65536"] {
            let error = configured(Some(bad), Some("::1")).unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
            assert!(error.to_string().contains("RACER_METRICS_ADDR"));
        }
        for setting in ["RACER_METRICS_ADDR", "RACER_POD_IP"] {
            let error = management_address(|name| {
                Err(if name == setting {
                    env::VarError::NotUnicode(std::ffi::OsString::from_vec(vec![0xff]))
                } else {
                    env::VarError::NotPresent
                })
            })
            .unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        }
    }

    fn probe(address: SocketAddr, path: &str) -> String {
        let timeout = Duration::from_secs(3);
        let mut socket = TcpStream::connect_timeout(&address, timeout).unwrap();
        socket.set_read_timeout(Some(timeout)).unwrap();
        socket.set_write_timeout(Some(timeout)).unwrap();
        write!(
            socket,
            "GET {path} HTTP/1.1\r\nHost: {address}\r\nConnection: close\r\n\r\n"
        )
        .unwrap();
        let mut response = String::new();
        socket.read_to_string(&mut response).unwrap();
        response
    }

    #[test]
    fn primary_ipv4_and_ipv6_pod_probes_reach_real_management_sockets() {
        // No io_uring/NUMA fixture and no IPv6 skip: these are actual exporter sockets.
        // Use ephemeral ports only after checking the production default port.
        for pod_ip in ["127.0.0.1", "::1"] {
            let mut address = configured(None, Some(pod_ip)).unwrap();
            assert_eq!(address.port(), 9090);
            address.set_port(0);
            check_socket(address, pod_ip.parse().unwrap());
        }
    }

    #[test]
    fn explicit_socket_overrides_pod_family_and_reports_assigned_port() {
        for (override_addr, pod_ip, connect_ip) in [
            ("127.0.0.1:0", "::1", "127.0.0.1"),
            ("[::1]:0", "127.0.0.1", "::1"),
            ("0.0.0.0:0", "::1", "127.0.0.1"),
            ("[::]:0", "127.0.0.1", "::1"),
        ] {
            check_socket(
                configured(Some(override_addr), Some(pod_ip)).unwrap(),
                connect_ip.parse().unwrap(),
            );
        }
    }

    fn check_socket(address: SocketAddr, connect_ip: IpAddr) {
        let updates = Arc::new(control::Updates::default());
        let life = Arc::new(lifecycle::Lifecycle::new(lifecycle::Config::default()));
        life.configure_workers(1);
        let registry =
            Arc::new(metrics::Registry::new(1, updates.clone()).with_lifecycle(life.clone()));
        let exporter = metrics::Exporter::start(address, registry.clone()).unwrap();
        let bound = exporter.address();
        assert_eq!(bound.ip(), address.ip());
        assert_ne!(bound.port(), 0);
        let endpoint = SocketAddr::new(connect_ip, bound.port());
        assert!(probe(endpoint, "/livez").starts_with("HTTP/1.1 503"));
        life.progress(0);
        let live = probe(endpoint, "/livez");
        assert!(live.starts_with("HTTP/1.1 200 OK\r\n"), "{live}");
        assert!(live.ends_with("\r\n\r\nok\n"));
        let ready = probe(endpoint, "/readyz");
        assert!(
            ready.starts_with("HTTP/1.1 503 Service Unavailable\r\n"),
            "{ready}"
        );
        let status = probe(endpoint, "/status");
        assert!(status.starts_with("HTTP/1.1 200 OK\r\n"), "{status}");
        for response in [ready, status] {
            let body = response.split_once("\r\n\r\n").unwrap().1;
            let mut expected = updates.status();
            expected["workerHealthy"] = true.into();
            expected["draining"] = false.into();
            assert_eq!(
                serde_json::from_str::<serde_json::Value>(body).unwrap(),
                expected
            );
        }
        let counters = probe(endpoint, "/metrics");
        assert!(counters.starts_with("HTTP/1.1 200 OK\r\n"), "{counters}");
        assert!(counters.contains("racer_dataplane_config_epoch 0\n"));
        let error = metrics::Exporter::start(bound, registry).err().unwrap();
        assert_eq!(error.kind(), io::ErrorKind::AddrInUse);
    }
}

#[cfg(test)]
mod startup_layout_tests {
    use super::*;
    use std::process::{Command, Stdio};
    use std::time::Instant;

    #[test]
    fn startup_layout_child() {
        let Ok(expected) = env::var("RACER_LAYOUT_EXPECT_ERROR") else {
            return;
        };
        let error = main_with_args(std::iter::empty::<std::ffi::OsString>())
            .unwrap_err()
            .to_string();
        assert!(error.contains(&expected), "expected {expected}: {error}");
    }

    #[test]
    fn actual_cpu_plan_is_persisted_and_incompatibility_precedes_listeners() {
        let plan = workers::CpuPlan::discover(
            workers::Config {
                shard_count: NonZeroUsize::new(32).unwrap(),
            },
            workers::WorkerCounts {
                io_per_node: NonZeroUsize::new(1),
                compute_per_node: NonZeroUsize::new(1),
            },
        )
        .unwrap();
        let path =
            env::temp_dir().join(format!("racer-startup-layout-{}.slab", std::process::id()));
        assert!(!path.exists());
        let size = 32u64 * 32 * 1024 * 1024;
        let run = |size: &str, shards: Option<&str>, expected: &str| {
            let mut command = Command::new(env::current_exe().unwrap());
            command
                .args([
                    "--exact",
                    "startup_layout_tests::startup_layout_child",
                    "--test-threads=2",
                ])
                .env_clear()
                .env("RACER_LAYOUT_EXPECT_ERROR", expected)
                .env("RACER_CONTROL_PLANE_URL", "/unused-bootstrap.json")
                .env("RACER_UNIVERSE", "01".repeat(32))
                .env("RACER_NODE", "02".repeat(32))
                .env("RACER_IO_WORKERS", "1")
                .env("RACER_COMPUTE_WORKERS", "1")
                .env("RACER_SLAB_SIZE", size)
                .env("RACER_SLAB_PATH", &path)
                // If layout validation is bypassed, the wrong error proves startup
                // reached listener setup. This avoids requiring NUMA pool allocation.
                .env("RACER_METRICS_ADDR", "invalid-listener")
                .stdout(Stdio::piped());
            if let Some(shards) = shards {
                command.env("RACER_SHARDS", shards);
            }
            let mut child = command.spawn().unwrap();
            let deadline = Instant::now() + Duration::from_secs(15);
            loop {
                if let Some(status) = child.try_wait().unwrap() {
                    let output = child.wait_with_output().unwrap();
                    assert!(
                        status.success(),
                        "{}",
                        String::from_utf8_lossy(&output.stdout)
                    );
                    assert!(String::from_utf8_lossy(&output.stdout).contains("1 passed; 0 failed"));
                    break;
                }
                if Instant::now() > deadline {
                    child.kill().unwrap();
                    child.wait().unwrap();
                    panic!("startup child timed out");
                }
                thread::sleep(Duration::from_millis(5));
            }
        };
        let size = size.to_string();
        let large = (2u64 << 40).to_string();
        run(&large, Some("32"), "517 bitmap pages");
        assert!(!path.exists());
        run(&size, Some("32"), "invalid RACER_METRICS_ADDR");
        // First startup persisted the discovered TOTAL, not the per-node setting.
        drop(Slab::open_or_create_layout(&path, 0, 32, plan.io().len()).unwrap());
        run(&large, Some("32"), "invalid RACER_METRICS_ADDR"); // Creation size is ignored on reopen.
        run(&large, None, "invalid RACER_METRICS_ADDR"); // Discover the old recorded count.
        run("not-a-size", Some("32"), "invalid RACER_METRICS_ADDR");
        std::fs::remove_file(&path).unwrap();
        let other = if plan.io().len() == 1 { 2 } else { 1 };
        drop(Slab::open_or_create_layout(&path, 32 * 32 * 1024 * 1024, 32, other).unwrap());
        run(
            &size,
            Some("32"),
            &format!("requires {other} total I/O workers"),
        );
        std::fs::remove_file(&path).unwrap();
        drop(Slab::create(&path, 32 * 32 * 1024 * 1024, 32).unwrap());
        run(&size, Some("32"), "missing user.racer.layout");
        run(&size, None, "missing user.racer.layout");
        std::fs::remove_file(&path).unwrap();
        run(&large, None, "invalid RACER_METRICS_ADDR");
        let automatic = Slab::open_existing_layout(&path, plan.io().len()).unwrap();
        assert_eq!(automatic.size(), 2 << 40);
        assert_eq!(automatic.shard_count(), 128);
        drop(automatic);
        run(&size, None, "invalid RACER_METRICS_ADDR");
        // A durable runtime layout wins over the old creation-only shard hint.
        run(&size, Some("32"), "invalid RACER_METRICS_ADDR");
        std::fs::remove_file(&path).unwrap();

        // Pre-planner layouts can have more than 1024 shards. Restart must
        // authorize the recorded geometry with the same worker count, without
        // rewriting the inode or using the creation-only shard/size hints.
        use std::os::unix::fs::MetadataExt;
        drop(Slab::open_or_create_layout(&path, 64 << 30, 2048, 1).unwrap());
        let inode = std::fs::metadata(&path).unwrap().ino();
        for size in ["10737418240", "not-a-size"] {
            run(size, Some("1"), "invalid RACER_METRICS_ADDR");
            assert_eq!(std::fs::metadata(&path).unwrap().ino(), inode);
            let persisted = Slab::open_existing_layout(&path, 1).unwrap();
            assert_eq!(persisted.size(), 64 << 30);
            assert_eq!(persisted.shard_count(), 2048);
        }
        assert!(Slab::open_existing_layout(&path, 2).is_err());
        std::fs::remove_file(&path).unwrap();

        // Exercise the shipping managed cap, not the standalone 32-worker cap.
        // Publish complete replacement inodes as the runtime does, then restart
        // through main with the unchanged managed 10 GiB/one-shard environment.
        let candidate = path.with_extension("slab.resize");
        for capacity in [10u64 << 30, 2 << 40, 4 << 40, 32 << 20] {
            drop(
                allocator::LayoutPlan::new(capacity, 1)
                    .unwrap()
                    .create(&candidate, allocator::CheckpointBudget::default())
                    .unwrap(),
            );
            std::fs::rename(&candidate, &path).unwrap();
            std::fs::File::open(path.parent().unwrap())
                .unwrap()
                .sync_all()
                .unwrap();
            // Interrupted private preparation is discarded on restart.
            std::fs::write(&candidate, b"interrupted candidate").unwrap();
            run("10737418240", Some("1"), "invalid RACER_METRICS_ADDR");
            assert!(!candidate.exists());
            let persisted = Slab::open_existing_layout(&path, 1).unwrap();
            assert_eq!(persisted.size(), capacity);
            assert_eq!(
                persisted.shard_count(),
                allocator::LayoutPlan::new(capacity, 1)
                    .unwrap()
                    .shard_count()
            );
        }
        // Neither a malformed placement xattr nor an unrelated file may be
        // silently adopted/reformatted just because creation hints are present.
        use std::os::fd::AsRawFd;
        let file = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
        let bad = b"malformed";
        // SAFETY: live descriptor, terminated name and bounded readable bytes.
        assert_eq!(
            unsafe {
                libc::fsetxattr(
                    file.as_raw_fd(),
                    c"user.racer.layout".as_ptr(),
                    bad.as_ptr().cast(),
                    bad.len(),
                    libc::XATTR_REPLACE,
                )
            },
            0
        );
        drop(file);
        run(
            "10737418240",
            Some("1"),
            "invalid or unsupported user.racer.layout",
        );
        assert_eq!(std::fs::metadata(&path).unwrap().len(), 32 << 20);
        std::fs::remove_file(&path).unwrap();
        std::fs::write(&path, b"unrelated state").unwrap();
        run("10737418240", Some("1"), "missing user.racer.layout");
        assert_eq!(std::fs::read(&path).unwrap(), b"unrelated state");
        std::fs::remove_file(&path).unwrap();
        let mut lock = path.as_os_str().to_owned();
        lock.push(".lock");
        std::fs::remove_file(lock).unwrap();
    }
}
