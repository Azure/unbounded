// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::RefCell;
use std::collections::{HashMap, HashSet};
use std::rc::Rc;

pub(crate) struct RegistryTransaction<P, T> {
    live: Rc<RefCell<HashMap<String, T>>>,
    replacements: HashMap<String, P>,
    removals: HashSet<String>,
}

impl<P, T> RegistryTransaction<P, T> {
    pub(crate) fn new(live: Rc<RefCell<HashMap<String, T>>>) -> Self {
        Self {
            live,
            replacements: HashMap::new(),
            removals: HashSet::new(),
        }
    }

    pub(crate) fn replace(&mut self, id: String, value: P) {
        self.removals.remove(&id);
        self.replacements.insert(id, value);
    }

    pub(crate) fn remove(&mut self, id: &str) {
        self.replacements.remove(id);
        self.removals.insert(id.to_string());
    }

    pub(crate) fn commit(
        self,
        mut activate: impl FnMut(&str, P) -> Result<T, String>,
    ) -> Result<(), String> {
        let mut replacements = HashMap::with_capacity(self.replacements.len());
        let mut prepared: Vec<_> = self.replacements.into_iter().collect();
        prepared.sort_by(|(a, _), (b, _)| a.cmp(b));
        for (id, value) in prepared {
            let active = activate(&id, value)?;
            replacements.insert(id, active);
        }

        let mut live = self.live.borrow_mut();
        for id in self.removals {
            live.remove(&id);
        }
        for (id, active) in replacements {
            live.insert(id, active);
        }
        Ok(())
    }
}
