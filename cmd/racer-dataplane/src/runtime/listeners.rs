// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Socket ownership and reconciliation, independent of generation authority.
use super::*;

impl Volumes {
    // Validate before bind/crypto attachment. A staged listener already accepts
    // kernel traffic, so rollback after a conflicting bind would be too late.
    pub(super) fn validate_listeners(&self, config: &Prepared) -> io::Result<()> {
        for (index, volume) in config.volumes().iter().enumerate() {
            let address = Address::Unix(volume.cache_socket());
            let conflict = config.volumes()[..index]
                .iter()
                .map(|v| (Address::Unix(v.cache_socket()), "candidate"))
                .chain(
                    self.staged
                        .iter()
                        .flat_map(|s| s.listeners.keys().map(|&a| (a, "staged"))),
                )
                .find(|&(other, _)| address == other);
            if let Some((other, state)) = conflict {
                return Err(io::Error::new(
                    io::ErrorKind::AddrInUse,
                    format!(
                        "volume {} listener {address} overlaps {state} listener {other}",
                        volume.config().id
                    ),
                ));
            }
        }
        Ok(())
    }
    // Move the whole server: outstanding accepts and tasks retain their ring and
    // pinned generations. Never bind a competitor or renew retired leases.
    fn reclaim_listener(&mut self, address: Address) {
        if let Some((_, server)) = self.retired.remove(&address) {
            assert!(!self.servers.contains_key(&address));
            self.servers.insert(address, server);
        }
    }
    fn receive_authority(&mut self, config: &Arc<Prepared>) {
        *self.receive_authority.borrow_mut() = Some(config.clone());
        if let Some(server) = &mut self.peer_server {
            server.handler_mut().config = Some(config.clone());
        }
    }
    pub(super) fn commit(&mut self, staged: Staged) {
        self.receive_authority(&staged.config);
        let Staged {
            generations,
            mut listeners,
            ..
        } = staged;
        let now = crate::environment::now();
        // All fallible work completed. No requests are polled during this swap.
        // Reconcile by address rather than volume ID to preserve accepted work.
        let removed: Vec<_> = self
            .servers
            .keys()
            .filter(|a| !generations.contains_key(a))
            .copied()
            .collect();
        for address in removed {
            let mut server = self.servers.remove(&address).unwrap();
            server.handler_mut().current.retire(now);
            assert!(
                self.retired
                    .insert(address, (now + DRAIN_TIMEOUT, server))
                    .is_none()
            );
        }
        for (address, current) in generations {
            self.reclaim_listener(address);
            current.active.set(true);
            if let Some(server) = self.servers.get_mut(&address) {
                let handler = server.handler_mut();
                handler
                    .draining
                    .retain(|g| !Rc::ptr_eq(g, &current) && !g.expired.get());
                if Rc::ptr_eq(&handler.current, &current) {
                    continue;
                }
                handler.current.retire(now);
                let old = std::mem::replace(&mut handler.current, current);
                if !old.expired.get() {
                    handler.draining.push(old);
                }
                if handler.draining.len() > MAX_DRAINING {
                    handler.draining.remove(0).expire();
                }
            } else {
                self.servers.insert(
                    address,
                    http::Server::new(
                        listeners.remove(&address).unwrap(),
                        VolumeHandler {
                            local: true,
                            current,
                            draining: Vec::new(),
                        },
                        http::Config::default(),
                    ),
                );
            }
        }
    }
    fn poll_retired(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let now = crate::environment::now();
        let mut work = uring::Work::default();
        let mut closed = Vec::new();
        for (address, (deadline, server)) in &mut self.retired {
            let reserved = self
                .staged
                .as_ref()
                .is_some_and(|stage| stage.generations.contains_key(address));
            if now >= *deadline && !reserved {
                server.handler_mut().expire();
                server.shutdown(ring)?;
                closed.push(*address);
            } else {
                // An unarmed stage reserves the socket, not old authority.
                work.merge(server.handler_mut().poll_background(ring, budget)?);
                work.merge(server.poll(ring, budget)?);
                if now < *deadline {
                    work.merge(uring::Work {
                        runnable: false,
                        deadline: Some(*deadline),
                    });
                }
            }
        }
        for address in closed {
            self.retired.remove(&address);
        }
        Ok(work)
    }
    pub(super) fn poll_listeners(
        &mut self,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<uring::Work> {
        let mut work = uring::Work::default();
        for server in self.servers.values_mut() {
            work.merge(server.poll(ring, budget)?);
            work.merge(server.handler_mut().poll_background(ring, budget)?);
        }
        work.merge(self.poll_retired(ring, budget)?);
        if let Some(peer) = &mut self.peer_server {
            let volumes = &mut peer.handler_mut().volumes;
            volumes.clear();
            for server in self
                .retired
                .values()
                .map(|(_, s)| s)
                .chain(self.servers.values())
            {
                let handler = server.handler();
                for generation in std::iter::once(&handler.current).chain(&handler.draining) {
                    if generation.expired.get() {
                        continue;
                    }
                    match volumes.entry(generation.volume.clone()) {
                        std::collections::btree_map::Entry::Vacant(entry) => {
                            entry.insert(VolumeHandler {
                                local: false,
                                current: generation.clone(),
                                draining: Vec::new(),
                            });
                        }
                        std::collections::btree_map::Entry::Occupied(mut entry) => {
                            let target = entry.get_mut();
                            if generation._config.config_snapshot().revision
                                > target.current._config.config_snapshot().revision
                            {
                                let old =
                                    std::mem::replace(&mut target.current, generation.clone());
                                target.draining.push(old);
                            } else {
                                target.draining.push(generation.clone());
                            }
                        }
                    }
                }
            }
            work.merge(peer.poll(ring, budget)?);
            work.merge(uring::Work {
                runnable: false,
                deadline: Some(crate::environment::now() + Duration::from_secs(1)),
            });
        }
        if self.retired.is_empty()
            && self
                .servers
                .values_mut()
                .all(|s| s.handler_mut().draining.is_empty())
        {
            self.updates.retired(self.revision, self.worker);
        }
        Ok(work)
    }
}
