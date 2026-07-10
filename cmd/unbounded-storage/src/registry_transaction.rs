// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;

pub(crate) struct RegistryTransaction<T> {
    live: Rc<RefCell<HashMap<String, T>>>,
    originals: HashMap<String, Option<T>>,
    completed: bool,
}

impl<T> RegistryTransaction<T> {
    pub(crate) fn new(live: Rc<RefCell<HashMap<String, T>>>) -> Self {
        Self {
            live,
            originals: HashMap::new(),
            completed: false,
        }
    }

    pub(crate) fn replace(&mut self, id: String, value: T) {
        self.record_original(&id);
        self.live.borrow_mut().insert(id, value);
    }

    pub(crate) fn remove(&mut self, id: &str) {
        self.record_original(id);
        self.live.borrow_mut().remove(id);
    }

    pub(crate) fn finalize(mut self) {
        self.completed = true;
        self.originals.clear();
    }

    pub(crate) fn rollback(mut self) {
        self.rollback_inner();
        self.completed = true;
    }

    fn record_original(&mut self, id: &str) {
        if self.originals.contains_key(id) {
            return;
        }
        let original = self.live.borrow_mut().remove(id);
        self.originals.insert(id.to_string(), original);
    }

    fn rollback_inner(&mut self) {
        let mut live = self.live.borrow_mut();
        for (id, original) in self.originals.drain() {
            live.remove(&id);
            if let Some(original) = original {
                live.insert(id, original);
            }
        }
    }
}

impl<T> Drop for RegistryTransaction<T> {
    fn drop(&mut self) {
        if !self.completed {
            self.rollback_inner();
        }
    }
}
