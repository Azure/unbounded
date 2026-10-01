//! Seeded action scheduling and terminal byte/resource oracle obligations.
use super::*;

impl Client {
    pub(super) fn poll(&mut self) -> bool {
        if self.sent == 0
            && !self.head
            && self.expected_status.is_none()
            && self.request.starts_with(b"GET ")
        {
            let request = String::from_utf8(self.request.clone()).unwrap();
            self.request = request.replacen("GET /v1/", "POST /v2/", 1)
                .replacen("Host: racer\r\n", "Host: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: 64\r\nRacer-Byte-Credits: 1073741824\r\nRacer-Ordered: 1\r\n", 1).into_bytes();
        }
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
    pub(super) fn generated(&mut self, steps: usize) {
        for object in 0..8 {
            self.update(object);
        }
        let initial = 2 + self.rng.pick(MAX_NODES - 1);
        for _ in 0..initial {
            self.add(None);
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
            match action {
                Action::AddNode if self.nodes.len() < MAX_NODES => self.add(None),
                Action::RemoveNode if self.nodes.len() > 1 => {
                    let index = self.rng.pick(self.nodes.len());
                    self.remove(index);
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
                    let id = self.remove(index);
                    self.add(Some(id));
                }
                Action::ShortIo => {
                    let operation = if self.rng.pick(2) == 0 {
                        "send"
                    } else {
                        "recv"
                    };
                    self.sim
                        .inject(operation, Fault::Short(1 + self.rng.pick(128)));
                    *self.coverage.injected.entry(operation.into()).or_default() += 1;
                    self.coverage.action("short-io");
                    self.traffic(1, false);
                }
                Action::ConnectFailure => {
                    self.sim.inject("connect", Fault::Errno(libc::ECONNREFUSED));
                    *self.coverage.injected.entry("connect".into()).or_default() += 1;
                    // A fresh unpinned object requires an origin connection even
                    // when every peer already has a cached copy of the old version.
                    self.update(2);
                    let client = self.request(2, false, false);
                    self.exchange(client, true);
                    self.coverage.action("connect-failure");
                    self.recover_origin();
                }
                Action::DelayedWrite => {
                    self.sim.inject("write", Fault::Delay(3 + self.rng.pick(8)));
                    *self.coverage.injected.entry("write".into()).or_default() += 1;
                    self.update(3);
                    let client = self.request(3, false, false);
                    self.exchange(client, false);
                    self.coverage.action("delayed-write");
                }
                Action::ClientCancel => {
                    let mut client = self.request(1, false, false);
                    // Send the request without consuming the response, ensuring
                    // real socket backpressure regardless of the generated path.
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
                Action::PeerOutage => self.peer_outage(),
                Action::InflightCrash => {
                    self.crash_inflight();
                }
                Action::OldPin => {
                    // Warm the ingress before mutation. This old pin is provably
                    // retained, so unavailability cannot excuse a failed read.
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
                    self.sim.inject("write", Fault::Errno(libc::EIO));
                    *self.coverage.injected.entry("write".into()).or_default() += 1;
                    self.update(4);
                    let client = self.request(4, false, false);
                    self.exchange(client, false);
                    self.coverage.action("failed-dirty-write");
                }
                Action::InflightMembership if self.nodes.len() < MAX_NODES => {
                    let mut client = self.request(1, false, false);
                    self.tick();
                    let complete = client.poll();
                    self.add(None);
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
                    let wall = crate::runtime::environment::wall_now();
                    let amount = Duration::from_secs(1 + self.rng.pick(120) as u64);
                    self.clock.set_wall_time(if self.rng.pick(2) == 0 {
                        wall + amount
                    } else {
                        wall - amount
                    });
                    self.traffic(1, true);
                    // Replay admission retains a wall high-water mark. Recovery
                    // advances past it; rolling back again is not a healthy clock.
                    self.clock.advance(Duration::from_secs(121));
                    let (anchor, wall_anchor) = crate::runtime::environment::clock_anchor();
                    self.clock.set_wall_time(
                        wall_anchor + crate::runtime::environment::now().duration_since(anchor),
                    );
                    self.coverage.action("wall-jump");
                }
                Action::OriginFault => self.origin_fault(),
                Action::MalformedClient => self.malformed_client(),
                Action::KeyRetirement => self.key_retirement(),
                Action::CacheRecreate => self.cache_recreate(),
                Action::NativeFault if self.native => self.native_fault(),
                Action::PeerSecurity if self.nodes.len() > 1 => self.peer_security(),
                Action::DiskCorruption => self.disk_corruption(),
                Action::PendingWriteCrash => self.crash_pending_write(),
                // Gated actions keep their slot and use ordinary traffic instead
                // of resampling, preserving both weights and random draws.
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
            self.coverage.trace.checkpoint();
        }
        // Recovery liveness is a mandatory oracle obligation, independent of the
        // generator's action mix: every current object must still be readable.
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
        while !self.nodes.is_empty() {
            self.remove(0);
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
