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
            format!("{method} /v2/objects/{} HTTP/1.1\r\n{fields}\r\n", key(2)).into_bytes();
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

    pub(super) fn peer_security(&mut self) {
        use crate::{
            http::{Codec, MessageHead, StartLine},
            peer::protocol::encode_envelope,
            security::protocol as p,
        };
        let receiver = self.rng.pick(self.nodes.len());
        let sender = (receiver + 1) % self.nodes.len();
        let keys = self.nodes[sender].workers[0].app.keys.clone();
        let certificates = Rc::new(Certificates::new(
            self.nodes[sender].config.cluster.clone(),
            keys.clone(),
        ));
        let signatures = Signatures::new(keys, certificates.clone());
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
        let envelope = crate::security::forwarding::ForwardedHead {
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
            let mut signed = crate::security::connection::signature_tests::clone_head(original);
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
                &crate::security::forwarding::ForwardedHead {
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
        use uring_runtime::reactor::simulation::DiskState;
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
                let object = usize::from_str_radix(&page.version.object.key.to_hex(), 16).unwrap();
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
        client.request = format!("GET /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nIf-Match: {}\r\nRange: bytes={}-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", key(object), client.tag, client.first, client.end - 1).into_bytes();
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

    pub(super) fn peer_outage(&mut self) {
        let index = self.rng.pick(self.nodes.len());
        let address = self.nodes[index].config.peer_listen;
        let listener_scope = self.nodes[index].workers[0]
            .app
            .listener_scope
            .take()
            .unwrap();
        listener_scope.cancel().unwrap();
        self.nodes[index].workers[0].app.peer_task.take();
        let endpoint = crate::http::connection::Endpoint::Peer(address.to_string());
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
        let listener_scope = scope(Duration::from_secs(365 * 24 * 3600)).unwrap();
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

    pub(super) fn crash_inflight(&mut self) {
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
        let id = self.retire(node, true);
        self.exchange(client, true);
        self.add(Some(id));
        self.coverage.action("inflight-crash");
    }

    pub(super) fn crash_pending_write(&mut self) {
        self.update(7);
        self.sim.inject("write", Fault::Delay(64));
        *self.coverage.injected.entry("write".into()).or_default() += 1;
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
        let id = self.retire(victim, true);
        if !completed {
            self.exchange(client, true);
        }
        self.add(Some(id));
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

    pub(super) fn cache_recreate(&mut self) {
        self.generation += 1;
        let members = self.members();
        let mut staged = Vec::new();
        for node in &mut self.nodes {
            node.workers[0].app.control = node.control.clone();
            staged.push(caches::CachePublication {
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
                            test_support::publication(&node.config, self.generation, vec![]);
                        publication.membership_version = MembershipVersion(self.generation);
                        publication.members = members.clone();
                        node.workers[0]
                            .app
                            .snapshots
                            .publish_staged(publication, Some(transition))
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
                app.clients
                    .reconcile(&definitions, &scope(Duration::from_secs(30)).unwrap()),
            )
            .unwrap();
        }
        self.coverage.action("cache-retire-recreate");
    }

    pub(super) fn native_fault(&mut self) {
        use crate::rdma::lifecycle::simulation::{
            Fault as NativeFault, Operation as NativeOperation,
        };
        if self.native_rules.is_empty() {
            for op in [
                NativeOperation::Bind,
                NativeOperation::Write,
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
            self.add(None);
        }
        self.settle();
        self.clock.advance(Duration::from_secs(26));
        let object = self.object_id(5);
        let candidates = self.ranked_nodes(&object);
        let source = candidates[0];
        let receiver = candidates[1];
        self.update(5);
        // Candidates probe predecessors CopyOnly before filling from origin.
        // Warm the primary's exact version first, including in two-node clusters.
        // The byte oracle remains independent of this injection-path selection.
        let warm = self.request_on(5, true, false, source);
        self.exchange(warm, false);
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
                }
            ))
            .or_default() += 1;
        self.coverage.action("native-fault-observed");
    }
}
