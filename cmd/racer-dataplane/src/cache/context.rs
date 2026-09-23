// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::Namespace;
use std::{cell::RefCell, rc::Rc};

/// Immutable admission context for one volume generation. Clones share the
/// checksum worker, not mutable routing or cache configuration. Metadata retains
/// this context so pages admitted later in a stream use the original worker.
#[derive(Clone)]
pub struct Context {
    namespace: Namespace,
    pub(super) authorization: crate::authorization::Authorization,
    pub(super) crypto: Option<Rc<RefCell<crate::crypto::Worker>>>,
}

impl Context {
    pub fn new(namespace: Namespace) -> Self {
        Self {
            namespace,
            authorization: Default::default(),
            crypto: None,
        }
    }

    pub fn with_crypto(mut self, crypto: Option<Rc<RefCell<crate::crypto::Worker>>>) -> Self {
        self.crypto = crypto;
        self
    }

    pub fn namespace(&self) -> Namespace {
        self.namespace
    }
    pub fn with_authorization(
        mut self,
        authorization: crate::authorization::Authorization,
    ) -> Self {
        self.authorization = authorization;
        self
    }
}
