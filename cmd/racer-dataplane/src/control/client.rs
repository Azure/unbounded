//! TLS control bootstrap, authenticated stream resume, enrollment, and publications.
use super::{
    caches::CacheRegistry, enrollment::Enrollment, secrets::SecretWatcher, snapshot::SnapshotStore,
};
use crate::{
    error::{Operation, deferred},
    runtime::deadline::RequestScope,
};
use std::rc::Rc;
pub struct ControlClient {
    endpoint: String,
    enrollment: Enrollment,
    secrets: SecretWatcher,
    snapshots: Rc<SnapshotStore>,
    caches: Rc<CacheRegistry>,
}
impl ControlClient {
    pub fn new(
        endpoint: String,
        enrollment: Enrollment,
        secrets: SecretWatcher,
        snapshots: Rc<SnapshotStore>,
        caches: Rc<CacheRegistry>,
    ) -> Self {
        Self {
            endpoint,
            enrollment,
            secrets,
            snapshots,
            caches,
        }
    }
    pub fn run<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("control.run")
    }
}
#[cfg(test)]
mod tests { /* Server-authenticated bootstrap, mTLS renewal, reconnect, expired state. */
}
