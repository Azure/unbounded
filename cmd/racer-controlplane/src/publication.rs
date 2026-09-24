// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Validation and replacement of disposable in-memory publications. Revision
//! reservation and leadership authority belong to the Kubernetes runtime.

use std::sync::Arc;

use crate::model::Generation;
use crate::topology::Topology;
use crate::{Error, Result};

pub trait Versioned: Clone + PartialEq {
    fn validate(&self) -> Result<()>;
    fn revision(&self) -> u64;
    fn set_revision(&mut self, revision: u64);
    fn same_record(&self, other: &Self) -> bool;
}

impl Versioned for Generation {
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

pub struct Publication<T: Versioned> {
    published: Option<Arc<T>>,
}

impl<T: Versioned> Default for Publication<T> {
    fn default() -> Self {
        Self { published: None }
    }
}

impl<T: Versioned> Publication<T> {
    pub fn published(&self) -> Option<&Arc<T>> {
        self.published.as_ref()
    }

    /// Call only after reserving the revision and verifying current authority.
    /// Invalid input leaves the running last-good value intact. A new process
    /// starts empty and can only rebuild from current complete inventory.
    pub fn publish(&mut self, mut desired: T, revision: u64) -> Result<Arc<T>> {
        if revision == 0
            || self
                .published
                .as_ref()
                .is_some_and(|old| revision <= old.revision() || !old.same_record(&desired))
        {
            return Err(Error(
                "publication revision regressed or identity changed".into(),
            ));
        }
        desired.set_revision(revision);
        desired.validate()?;
        let next = Arc::new(desired);
        self.published = Some(next.clone());
        Ok(next)
    }
}
