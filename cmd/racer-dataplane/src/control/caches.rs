//! Cache definitions and lifecycle events. Removal drains sockets, keys, and I/O.
//!
//! Socket paths are fixed: /run/racer/<cache name>/client/socket and
//! /run/racer/<cache name>/origin/socket. Separate endpoint directories let pods
//! mount only the endpoint authorized by a future admission controller. The
//! dataplane owns the client listener; the application adapter owns the origin
//! listener. Never unlink an adapter-owned origin socket during cache removal.
use crate::{
    error::{Error, Result},
    model::identity::CacheId,
};
use std::{
    cell::RefCell,
    collections::{BTreeMap, HashSet},
    path::PathBuf,
};
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CacheDefinition {
    pub id: CacheId,
    /// ClusterCache name, distinct from its UID and one safe path component.
    pub name: String,
    /// Must equal /run/racer/<name>/client/socket, not an arbitrary supplied path.
    pub client_socket: PathBuf,
    /// Must equal /run/racer/<name>/origin/socket, not an arbitrary supplied path.
    pub origin_socket: PathBuf,
    pub socket_mode: u32,
}
pub enum CacheEvent {
    Add(CacheDefinition),
    Update(CacheDefinition),
    Remove(CacheId),
}
/// A sole owner stages all listener/resource changes before publication. Dropping
/// an uncommitted transition must undo preparation. Commit cannot fail; removal
/// stops admission and arranges drain/fences before releasing old resources.
pub trait CacheTransition {
    fn commit(self: Box<Self>);
}
pub trait CacheLifecycle {
    fn stage(&self, definitions: &[CacheDefinition]) -> Result<Box<dyn CacheTransition>>;
}
pub struct CacheRegistry {
    current: RefCell<BTreeMap<CacheId, CacheDefinition>>,
}
// Preserve side-effect-free scaffold construction syntax.
#[allow(non_upper_case_globals)]
pub const CacheRegistry: CacheRegistry = CacheRegistry {
    current: RefCell::new(BTreeMap::new()),
};
impl Default for CacheRegistry {
    fn default() -> Self {
        CacheRegistry
    }
}
pub fn canonical_socket_paths(name: &str) -> Result<(PathBuf, PathBuf)> {
    if name.is_empty()
        || name.len() > 253
        || !name.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && label.bytes().enumerate().all(|(i, b)| {
                    b.is_ascii_lowercase()
                        || b.is_ascii_digit()
                        || b == b'-' && i != 0 && i + 1 != label.len()
                })
        })
    {
        return Err(Error::InvalidRequest);
    }
    let client = format!("/run/racer/{name}/client/socket");
    let origin = format!("/run/racer/{name}/origin/socket");
    if client.len() > 107 || origin.len() > 107 {
        return Err(Error::InvalidRequest);
    }
    Ok((client.into(), origin.into()))
}
pub fn validate_definitions(definitions: &[CacheDefinition]) -> Result<()> {
    let mut ids = HashSet::new();
    let mut names = HashSet::new();
    for d in definitions {
        if !super::wire::valid_uuid(&d.id.0)
            || !ids.insert(&d.id)
            || !names.insert(&d.name)
            || d.socket_mode > 0o777
        {
            return Err(Error::InvalidRequest);
        }
        let (client, origin) = canonical_socket_paths(&d.name)?;
        // Path equality normalizes separators; the wire requires exact strings.
        if d.client_socket.as_os_str() != client.as_os_str()
            || d.origin_socket.as_os_str() != origin.as_os_str()
        {
            return Err(Error::InvalidRequest);
        }
    }
    Ok(())
}
impl CacheRegistry {
    /// Reject unsafe/duplicate names, noncanonical paths, and socket paths exceeding
    /// the platform UDS limit. Validate before filesystem access; prevent symlink
    /// traversal outside the endpoint directories during socket lifecycle work.
    pub fn reconcile(&self, definitions: &[CacheDefinition]) -> Result<Vec<CacheEvent>> {
        validate_definitions(definitions)?;
        let next: BTreeMap<_, _> = definitions
            .iter()
            .map(|d| (d.id.clone(), d.clone()))
            .collect();
        let mut current = self.current.borrow_mut();
        let mut events = Vec::new();
        // Removals precede additions, including replacement of a reused cache name.
        for id in current.keys() {
            if !next.contains_key(id) {
                events.push(CacheEvent::Remove(id.clone()));
            }
        }
        for (id, d) in &next {
            match current.get(id) {
                None => events.push(CacheEvent::Add(d.clone())),
                Some(old) if old != d => events.push(CacheEvent::Update(d.clone())),
                _ => (),
            }
        }
        *current = next;
        Ok(events)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn replacement_removes_before_add_and_invalid_update_is_atomic() {
        let registry = CacheRegistry;
        let mut defs =
            crate::control::wire::decode_publication(include_bytes!("testdata/publication.json"))
                .unwrap()
                .caches;
        assert!(matches!(
            registry.reconcile(&defs).unwrap().as_slice(),
            [CacheEvent::Add(_)]
        ));
        assert!(registry.reconcile(&defs).unwrap().is_empty());
        let old = defs[0].id.clone();
        defs[0].id.0 = "66666666-6666-4666-8666-666666666666".into();
        let events = registry.reconcile(&defs).unwrap();
        assert!(matches!(&events[0],CacheEvent::Remove(id) if *id == old));
        assert!(matches!(&events[1], CacheEvent::Add(_)));
        let mut bad = defs.clone();
        bad[0].name = "..".into();
        assert!(registry.reconcile(&bad).is_err());
        assert!(registry.reconcile(&defs).unwrap().is_empty());
        defs[0].socket_mode = 0o600;
        assert!(matches!(
            registry.reconcile(&defs).unwrap().as_slice(),
            [CacheEvent::Update(_)]
        ));
        for name in ["", ".", "..", "a/b", "A", "-a", "a-", "a..b"] {
            assert!(canonical_socket_paths(name).is_err());
        }
        let maximum = format!("{}.{}", "a".repeat(63), "b".repeat(18));
        assert_eq!(
            canonical_socket_paths(&maximum)
                .unwrap()
                .0
                .as_os_str()
                .len(),
            107
        );
        assert!(canonical_socket_paths(&(maximum + "b")).is_err());
    }
}
