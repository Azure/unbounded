// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! One lock owns desired replacement, local commit grants, and active publication.
use super::{Prepared, invalid};
use std::{collections::BTreeSet, io, sync::Arc};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Decision {
    Waiting,
    Discard,
    Activate,
}

#[derive(Default)]
struct Activation {
    ready: BTreeSet<usize>,
    activated: BTreeSet<usize>,
    failed: BTreeSet<usize>,
    retired: BTreeSet<usize>,
}

#[derive(Default)]
pub(super) struct Coordinator {
    candidate: Option<Arc<Prepared>>,
    active: Option<Arc<Prepared>>,
    activation: Activation,
    workers: usize,
    committing: bool,
    queued: Option<Prepared>,
}
impl Coordinator {
    pub(super) fn local_state(&self) -> &'static str {
        if !self.activation.failed.is_empty() {
            "failed"
        } else if self
            .active
            .as_ref()
            .is_some_and(|p| p.config_snapshot().revision == self.revision())
        {
            "applied"
        } else if self.committing {
            "committing"
        } else {
            "preparing"
        }
    }
    /// Supersede unfinished preparation, but finish a granted local commit before
    /// replacing its candidate. Only the newest queued revision is retained.
    pub(super) fn desired(
        &mut self,
        next: Prepared,
        before_replace: impl FnOnce(),
    ) -> io::Result<()> {
        let revision = next.config_snapshot().revision;
        if let Some(old) = self.queued.as_ref().or(self.candidate.as_deref()) {
            if revision < old.config_snapshot().revision {
                return Err(invalid("configuration rollback"));
            }
            if revision == old.config_snapshot().revision {
                return if next.config_snapshot() == old.config_snapshot() {
                    Ok(())
                } else {
                    Err(invalid("revision reused for different contents"))
                };
            }
        }
        // Failed candidates never become the validation baseline. A commit grant
        // is irreversible, so a queued successor must also follow that candidate.
        if let Some(active) = &self.active {
            next.validate_successor(active)?;
        }
        if self.committing
            && let Some(candidate) = &self.candidate
        {
            next.validate_successor(candidate)?;
        }
        before_replace();
        if self.committing && self.activation.activated.len() != self.workers {
            self.queued = Some(next);
        } else {
            self.replace_desired(next);
        }
        Ok(())
    }
    fn replace_desired(&mut self, next: Prepared) {
        self.activation = Activation::default();
        self.committing = false;
        self.candidate = Some(Arc::new(next));
        if self.workers == 0 {
            self.active = self.candidate.clone();
        }
    }
    pub(super) fn revision(&self) -> u64 {
        self.candidate
            .as_ref()
            .map_or(0, |p| p.config_snapshot().revision)
    }
    pub(super) fn subscribe(&mut self) {
        self.workers += 1;
    }
    pub(super) fn latest(&self, revision: u64) -> Option<Arc<Prepared>> {
        (self.revision() != revision)
            .then(|| self.candidate.clone())
            .flatten()
    }
    pub(super) fn active(&self) -> Option<Arc<Prepared>> {
        self.active.clone()
    }
    pub(super) fn applied_epoch(&self) -> u64 {
        self.active
            .as_ref()
            .map_or(0, |p| p.config_snapshot().epoch)
    }
    pub(super) fn staged(&mut self, revision: u64, worker: usize, success: bool) {
        if worker >= self.workers
            || revision != self.revision()
            || self.activation.ready.contains(&worker)
        {
            return;
        }
        if success {
            self.activation.ready.insert(worker);
            self.activation.failed.remove(&worker);
        } else {
            self.activation.failed.insert(worker);
        }
    }
    pub(super) fn decision(&mut self, revision: u64) -> Decision {
        if self.candidate.is_none() || revision != self.revision() {
            Decision::Discard
        } else if self.activation.failed.is_empty() && self.activation.ready.len() == self.workers {
            self.committing = true;
            Decision::Activate
        } else {
            Decision::Waiting
        }
    }
    pub(super) fn activated(
        &mut self,
        revision: u64,
        worker: usize,
        before_publish: impl FnOnce(),
    ) {
        if worker >= self.workers
            || self.decision(revision) != Decision::Activate
            || !self.activation.activated.insert(worker)
        {
            return;
        }
        if self.activation.activated.len() == self.workers {
            before_publish();
            self.active = self.candidate.clone();
            if let Some(next) = self.queued.take() {
                self.replace_desired(next);
            }
        }
    }
    pub(super) fn retired(&mut self, revision: u64, worker: usize) {
        if revision == self.revision() && self.activation.activated.contains(&worker) {
            self.activation.retired.insert(worker);
        }
    }
    pub(super) fn status(&self) -> serde_json::Value {
        let active = &self.active;
        let a = &self.activation;
        let ready = active
            .as_ref()
            .is_some_and(|p| p.config_snapshot().idle || !p.volumes().is_empty());
        serde_json::json!({
            "ready": ready,
            "activeRevision": active.as_ref().map_or(0, |p| p.config_snapshot().revision),
            "candidateRevision": self.revision(), "localState": self.local_state(),
            "retiredWorkers": a.retired.len(), "preparedWorkers": a.ready.len(),
            "activatedWorkers": a.activated.len(), "workers": self.workers, "rejected": !a.failed.is_empty(),
            "volumes": active.as_ref().map(|p| p.volumes().iter().map(|v| serde_json::json!({"id":v.config().id,"epoch":v.config().topology.as_ref().map_or(0, |t|t.epoch),"ready":true})).collect::<Vec<_>>()).unwrap_or_default(),
        })
    }
}
