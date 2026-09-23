// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! One lock owns candidate replacement, worker barriers, and active publication.
use super::{Prepared, invalid};
use std::{collections::BTreeSet, io, sync::Arc};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Decision {
    Waiting,
    Discard,
    Activate,
}

/// Numeric values are the control protocol, not an ordering for Abort.
#[derive(Clone, Copy, Debug, Default, Eq, Ord, PartialEq, PartialOrd)]
#[repr(u32)]
enum Phase {
    #[default]
    Uncommanded = 0,
    Prepare = 1,
    Receive = 2,
    Transmit = 3,
    Retire = 4,
}
enum Command {
    Advance(Phase),
    Abort,
}
impl TryFrom<u32> for Command {
    type Error = io::Error;
    fn try_from(code: u32) -> io::Result<Self> {
        Ok(match code {
            1 => Self::Advance(Phase::Prepare),
            2 => Self::Advance(Phase::Receive),
            3 => Self::Advance(Phase::Transmit),
            4 => Self::Advance(Phase::Retire),
            5 => Self::Abort,
            _ => return Err(invalid("unknown control phase")),
        })
    }
}

#[derive(Default)]
struct Activation {
    ready: BTreeSet<usize>,
    activated: BTreeSet<usize>,
    failed: BTreeSet<usize>,
    aborted: bool,
    phase: Phase,
    received: BTreeSet<usize>,
    retired: BTreeSet<usize>,
    // A command's receive commitment prohibits Abort. This separate latch records
    // worker serving authority and is the boundary for forward correction.
    receive_granted: bool,
}
impl Activation {
    fn rejected(&self) -> bool {
        self.aborted || !self.failed.is_empty()
    }
    fn command(&mut self, command: Command) -> io::Result<()> {
        match command {
            Command::Abort if self.phase >= Phase::Receive => {
                return Err(invalid("cannot abort after receive commitment"));
            }
            Command::Abort => self.aborted = true,
            Command::Advance(_) if self.aborted => return Err(invalid("configuration aborted")),
            Command::Advance(phase) => self.phase = self.phase.max(phase),
        }
        Ok(())
    }
}

#[derive(Default)]
pub(super) struct Coordinator {
    candidate: Option<Arc<Prepared>>,
    active: Option<Arc<Prepared>>,
    activation: Activation,
    workers: usize,
    coordinated: bool,
}
impl Coordinator {
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
    pub(super) fn receive_pending(&self) -> bool {
        self.activation.receive_granted && self.activation.retired.len() != self.workers
    }
    pub(super) fn forward_eligible(&self, unpublished: bool) -> bool {
        !self.activation.receive_granted
            || (unpublished && self.revision() > 0 && self.activation.retired.len() == self.workers)
    }
    fn correction_allowed(&self, from: u64, to: u64) -> bool {
        to > from
            && ((self.candidate.is_none() || from == self.revision())
                && !self.activation.receive_granted
                || (from > self.revision() && self.activation.retired.len() == self.workers))
    }
    pub(super) fn staged(&mut self, revision: u64, worker: usize, success: bool) {
        if revision != self.revision()
            || self.activation.aborted
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
    pub(super) fn decision(&self, revision: u64) -> Decision {
        let a = &self.activation;
        if revision != self.revision() || a.aborted {
            Decision::Discard
        } else if !a.rejected()
            && a.ready.len() == self.workers
            && (!self.coordinated
                || (a.phase >= Phase::Transmit && a.received.len() == self.workers))
        {
            Decision::Activate
        } else {
            Decision::Waiting
        }
    }
    pub(super) fn receive_decision(&mut self, revision: u64) -> bool {
        let granted = revision == self.revision()
            && self.coordinated
            && !self.activation.rejected()
            && self.activation.phase >= Phase::Receive
            && self.activation.ready.len() == self.workers;
        self.activation.receive_granted |= granted;
        granted
    }
    pub(super) fn received(&mut self, revision: u64, worker: usize) {
        if revision == self.revision() && !self.activation.aborted {
            self.activation.received.insert(worker);
        }
    }
    pub(super) fn activated(
        &mut self,
        revision: u64,
        worker: usize,
        before_publish: impl FnOnce(),
    ) {
        if self.decision(revision) != Decision::Activate {
            return;
        }
        self.activation.activated.insert(worker);
        if self.activation.activated.len() == self.workers {
            // The caller holds the coordinator lock through this hook and swap.
            // Neither a successor nor status can observe a half-published state.
            before_publish();
            self.active = self.candidate.clone();
        }
    }
    pub(super) fn retired(&mut self, revision: u64, worker: usize) {
        if revision == self.revision() && self.activation.activated.contains(&worker) {
            self.activation.retired.insert(worker);
        }
    }
    pub(super) fn command_phase(&mut self, revision: u64, phase: u32) -> io::Result<()> {
        let command = Command::try_from(phase)?;
        if revision != self.revision() {
            return Err(invalid("command revision mismatch"));
        }
        self.activation.command(command)
    }
    pub(super) fn command(
        &mut self,
        next: Prepared,
        phase: u32,
        forward: Option<u64>,
        before_replace: impl FnOnce(),
    ) -> io::Result<()> {
        let command = Command::try_from(phase)?;
        let revision = next.config_snapshot().revision;
        if forward.is_none()
            && self.coordinated
            && revision > self.revision()
            && self.activation.phase < Phase::Receive
        {
            self.activation.aborted = true;
        }
        self.coordinated = true;
        self.publish(next, forward, before_replace)?;
        self.activation.command(command)
    }
    pub(super) fn acknowledged_phase(&self) -> u32 {
        let a = &self.activation;
        if a.rejected() {
            return 0;
        }
        if a.activated.len() == self.workers {
            return if a.phase >= Phase::Retire && a.retired.len() == self.workers {
                4
            } else {
                3
            };
        }
        if a.received.len() == self.workers {
            return 2;
        }
        if a.ready.len() == self.workers {
            return 1;
        }
        0
    }
    pub(super) fn publish(
        &mut self,
        next: Prepared,
        forward: Option<u64>,
        before_replace: impl FnOnce(),
    ) -> io::Result<()> {
        let correction =
            forward.is_some_and(|r| self.correction_allowed(r, next.config_snapshot().revision));
        if forward.is_some() && !correction {
            return Err(invalid("forward correction has receive obligation"));
        }
        if let Some(old) = &self.candidate {
            if next.config_snapshot().revision < old.config_snapshot().revision {
                return Err(invalid("configuration rollback"));
            }
            if next.config_snapshot().revision == old.config_snapshot().revision {
                return if next.config_snapshot() == old.config_snapshot() {
                    Ok(())
                } else {
                    Err(invalid("revision reused for different contents"))
                };
            }
            let a = &self.activation;
            if self.coordinated
                && !correction
                && (!a.rejected() || a.phase >= Phase::Receive)
                && a.retired.len() != self.workers
            {
                return Err(io::Error::new(
                    io::ErrorKind::WouldBlock,
                    "previous generation still draining",
                ));
            }
            if !correction && !a.rejected() && a.activated.len() != self.workers {
                return Err(io::Error::new(
                    io::ErrorKind::WouldBlock,
                    "previous snapshot still activating",
                ));
            }
            before_replace();
            next.validate_successor(old)?;
        }
        self.activation = Activation::default();
        self.candidate = Some(Arc::new(next));
        Ok(())
    }
    pub(super) fn status(&self) -> serde_json::Value {
        let active = &self.active;
        let a = &self.activation;
        let ready = active.as_ref().is_some_and(|p| {
            (p.config_snapshot().idle || !p.volumes().is_empty())
                && self.candidate.as_ref().is_none_or(|c| {
                    c.volumes().iter().all(|v| {
                        p.volumes().iter().any(|a| {
                            a.config().id == v.config().id && a.cache_socket() == v.cache_socket()
                        })
                    })
                })
        });
        serde_json::json!({
            "ready": ready,
            "activeRevision": active.as_ref().map_or(0, |p| p.config_snapshot().revision),
            "candidateRevision": self.revision(),
            "phase": a.phase as u32, "receiveReadyWorkers": a.received.len(),
            "retiredWorkers": a.retired.len(), "preparedWorkers": a.ready.len(),
            "activatedWorkers": a.activated.len(), "workers": self.workers, "rejected": a.rejected(),
            "volumes": active.as_ref().map(|p| p.volumes().iter().map(|v| serde_json::json!({"id":v.config().id,"epoch":v.config().topology.as_ref().map_or(0, |t|t.epoch),"ready":true})).collect::<Vec<_>>()).unwrap_or_default(),
        })
    }
}
