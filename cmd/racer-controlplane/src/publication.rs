// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::sync::Arc;

use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::model::Generation;
use crate::topology::Topology;
use crate::{Error, Result};

/// One independent CAS record. The adapter persists immutable bytes, then CASes
/// the pointer with both `expected_digest` and its external leadership fence.
pub trait Durable: Clone + PartialEq + Serialize {
    fn validate(&self) -> Result<()>;
    fn revision(&self) -> u64;
    fn set_revision(&mut self, revision: u64);
    fn same_record(&self, other: &Self) -> bool;
}

impl Durable for Generation {
    fn validate(&self) -> Result<()> {
        Topology::new(self).map(|_| ())
    }
    fn revision(&self) -> u64 {
        self.revision
    }
    fn set_revision(&mut self, revision: u64) {
        self.revision = revision;
    }
    fn same_record(&self, other: &Self) -> bool {
        self.universe == other.universe
    }
}

fn digest<T: Serialize>(value: &T) -> [u8; 32] {
    Sha256::digest(serde_json::to_vec(value).expect("durable model must serialize")).into()
}

/// Local completion fence, deliberately not a distributed phase or ledger.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Ticket(u64);

#[derive(Clone, Debug)]
pub struct Commit<T> {
    pub ticket: Ticket,
    pub expected_digest: Option<[u8; 32]>,
    pub digest: [u8; 32],
    pub value: Arc<T>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CommitOutcome {
    Committed,
    /// Definitely not committed; retry is safe from the same base.
    Rejected,
    /// Conflict or transport failure with an unknown durable outcome.
    ReloadRequired,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Completion {
    Published,
    Unchanged,
    ReloadRequired,
    Stale,
}

/// One instance per universe. Only authoritative load or successful commit can
/// replace published state. Keep serving last-good content while reloading.
pub struct Publication<T: Durable> {
    published: Option<Arc<T>>,
    pending: Option<Commit<T>>,
    sequence: u64,
    leader: bool,
    needs_reload: bool,
    reload: Option<Ticket>,
}

impl<T: Durable> Default for Publication<T> {
    fn default() -> Self {
        Self {
            published: None,
            pending: None,
            sequence: 0,
            leader: false,
            needs_reload: true,
            reload: None,
        }
    }
}

impl<T: Durable> Publication<T> {
    pub fn published(&self) -> Option<&Arc<T>> {
        self.published.as_ref()
    }
    pub fn needs_reload(&self) -> bool {
        self.needs_reload
    }

    fn ticket(&mut self) -> Ticket {
        self.sequence = self
            .sequence
            .checked_add(1)
            .expect("local completion sequence exhausted");
        Ticket(self.sequence)
    }

    /// Call after acquiring an externally fenced lease. Every acquisition,
    /// including reacquisition by the same process, requires authoritative load.
    pub fn acquire_leadership(&mut self) -> Ticket {
        self.leader = true;
        self.pending = None;
        self.needs_reload = true;
        self.begin_reload().expect("leadership was just acquired")
    }

    pub fn lose_leadership(&mut self) {
        self.leader = false;
        self.pending = None;
        self.reload = None;
        self.needs_reload = true;
    }

    pub fn begin_reload(&mut self) -> Result<Ticket> {
        if !self.leader || self.pending.is_some() {
            return Err(Error(
                "reload requires leadership and no pending commit".into(),
            ));
        }
        self.needs_reload = true;
        let ticket = self.ticket();
        self.reload = Some(ticket);
        Ok(ticket)
    }

    /// A failed read should not call this method; retry `begin_reload` instead.
    pub fn loaded(&mut self, ticket: Ticket, value: Option<T>) -> Result<Completion> {
        if !self.leader || self.reload != Some(ticket) {
            return Ok(Completion::Stale);
        }
        if let Some(next) = &value {
            next.validate()?;
        }
        if let Some(old) = &self.published {
            let Some(next) = &value else {
                return Err(Error("committed record disappeared".into()));
            };
            if !old.same_record(next)
                || next.revision() < old.revision()
                || (next.revision() == old.revision() && next != old.as_ref())
            {
                return Err(Error(
                    "authoritative record regressed or changed identity".into(),
                ));
            }
        }
        let changed = self.published.as_deref() != value.as_ref();
        self.published = value.map(Arc::new);
        self.reload = None;
        self.needs_reload = false;
        Ok(if changed {
            Completion::Published
        } else {
            Completion::Unchanged
        })
    }

    /// Returns None for unchanged content. The returned immutable value is the
    /// exact object to persist; it is not visible through `published` yet.
    pub fn prepare(&mut self, mut desired: T) -> Result<Option<Commit<T>>> {
        if !self.leader || self.needs_reload || self.pending.is_some() {
            return Err(Error(
                "publication requires a loaded leader with no pending commit".into(),
            ));
        }
        let revision = self.published.as_ref().map_or(0, |v| v.revision());
        desired.set_revision(revision);
        desired.validate()?;
        if let Some(old) = &self.published {
            if !old.same_record(&desired) {
                return Err(Error("publication record identity changed".into()));
            }
            if old.as_ref() == &desired {
                return Ok(None);
            }
        }
        desired.set_revision(
            revision
                .checked_add(1)
                .ok_or_else(|| Error("revision exhausted".into()))?,
        );
        let commit = Commit {
            ticket: self.ticket(),
            expected_digest: self.published.as_ref().map(|v| digest(v.as_ref())),
            digest: digest(&desired),
            value: Arc::new(desired),
        };
        self.pending = Some(commit.clone());
        Ok(Some(commit))
    }

    pub fn complete(&mut self, ticket: Ticket, outcome: CommitOutcome) -> Completion {
        if !self.leader || self.pending.as_ref().is_none_or(|c| c.ticket != ticket) {
            return Completion::Stale;
        }
        let commit = self.pending.take().unwrap();
        match outcome {
            CommitOutcome::Committed => {
                self.published = Some(commit.value);
                Completion::Published
            }
            CommitOutcome::Rejected => Completion::Unchanged,
            CommitOutcome::ReloadRequired => {
                self.needs_reload = true;
                Completion::ReloadRequired
            }
        }
    }
}
