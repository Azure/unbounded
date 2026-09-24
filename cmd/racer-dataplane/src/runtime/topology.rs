// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local preparation and atomic commit. Storage owns its
//! separate transaction and closes this fence while replacing cache generations.
use super::*;

#[derive(Default)]
pub(super) struct MaintenanceFence {
    held: bool,
}
impl MaintenanceFence {
    pub(super) fn set(&mut self, held: bool) {
        self.held = held;
    }
    pub(super) fn held(&self) -> bool {
        self.held
    }
}

impl Volumes {
    pub(super) fn poll_topology(&mut self, ring: &mut uring::Ring) -> io::Result<uring::Work> {
        let mut work = uring::Work::default();
        if self.topology_fence.held() {
            return Ok(work);
        }
        if !self.stopping
            && let Some(config) = self.updates.latest(self.revision)
        {
            self.staged = None;
            self.revision = config.config_snapshot().revision;
            self.preparing = Some((config, Retry::new(crate::environment::now())));
        }
        if let Some((config, mut retry)) = self.preparing.take()
            && self.updates.decision(self.revision) != Decision::Discard
        {
            // Subscription progress is independent of this timer. Successful
            // stages remain owned until decision and never repeat preparation.
            let now = crate::environment::now();
            if now >= retry.after {
                match self.prepare(config.clone(), ring) {
                    Ok(()) => self.updates.staged(self.revision, self.worker, true),
                    Err(error) => {
                        eprintln!("volume activation failed: {error}");
                        self.updates.staged(self.revision, self.worker, false);
                        retry.fail(crate::environment::now());
                    }
                }
            }
            if self.staged.is_none() {
                work.merge(uring::Work {
                    runnable: false,
                    deadline: Some(retry.after),
                });
                self.preparing = Some((config, retry));
            }
        }
        if let Some(staged) = self.staged.take() {
            match self.updates.decision(staged.revision) {
                Decision::Activate => {
                    self.commit(staged);
                    self.updates.activated(self.revision, self.worker);
                }
                Decision::Discard => {}
                Decision::Waiting => self.staged = Some(staged),
            }
        }
        Ok(work)
    }
    pub(super) fn prepare(
        &mut self,
        config: Arc<Prepared>,
        ring: &mut uring::Ring,
    ) -> io::Result<()> {
        self.validate_listeners(&config)?;
        self.provision_rdma(&config, ring)?;
        let crypto = {
            let (worker, source) = self.crypto.attach_local(ring.pool(), ring.wake_handle())?;
            let worker = Rc::new(RefCell::new(worker));
            self.crypto_sources.push((Rc::downgrade(&worker), source));
            Some(worker)
        };
        let mut generations = BTreeMap::new();
        let mut listeners = BTreeMap::new();
        for volume in config.volumes() {
            let address = Address::Unix(volume.cache_socket());
            if !self.servers.contains_key(&address) && !self.retired.contains_key(&address) {
                let path = volume.cache_socket();
                crate::socket_listener::SharedUnix::prepare_directory(path)?;
                listeners.insert(address, http::Listener::bind_unix(path)?);
            }
            let namespace = Namespace::volume(
                &config.config_snapshot().universe,
                &volume.config().id,
                volume.config().cache_generation,
                volume.backend().namespace(),
            );
            let mut handler =
                Handler::shared(self.cache.clone(), volume.backend().clone(), namespace);
            handler.set_crypto(crypto.clone());
            handler.set_authentication(config.authentication(&volume.config().id)?);
            handler.set_attempt_policy(volume.config().max_candidate_attempts.unwrap_or(3))?;
            handler.set_routing(
                volume.routing().clone(),
                volume
                    .config()
                    .peers
                    .iter()
                    .map(|id| (id.clone(), Peer::from_endpoint(volume.peers()[id].clone())))
                    .collect(),
            );
            if let Some(provider) = self.updates.credentials() {
                let universe = hex_identity(&config.config_snapshot().universe);
                let identities = config
                    .config_snapshot()
                    .peers
                    .iter()
                    .map(|peer| {
                        Ok((
                            peer.id.clone(),
                            crate::tls::PeerIdentity::new(&universe, &peer.id, &peer.pod_uid)?,
                        ))
                    })
                    .collect::<io::Result<BTreeMap<_, _>>>()?;
                handler.set_peer_tls(&volume.config().id, provider, &identities);
            }
            let generation = Rc::new(Generation {
                volume: volume.config().id.clone(),
                handler: Rc::new(RefCell::new(handler)),
                _config: config.clone(),
                manager: if let Some(rails) = &self.rails
                    && config.fabric().is_some()
                {
                    Some(RefCell::new(Manager {
                        context: Rc::new(
                            negotiation::Context::new(
                                config.clone(),
                                &volume.config().id,
                                self.worker as u64,
                                ROUTING,
                            )?
                            .with_credentials(self.updates.credentials())
                            .with_authority(self.receive_authority.clone()),
                        ),
                        rails: rails.clone(),
                        outbound: Vec::new(),
                        inbound: BTreeMap::new(),
                        live: Vec::new(),
                    }))
                } else {
                    None
                },
                active: Cell::new(false),
                drain: Cell::new(None),
                expired: Cell::new(false),
            });
            generations.insert(address, generation);
        }
        self.staged = Some(Staged {
            revision: config.config_snapshot().revision,
            config,
            generations,
            listeners,
        });
        Ok(())
    }
}
