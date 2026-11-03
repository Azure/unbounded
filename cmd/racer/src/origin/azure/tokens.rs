//! [`AzureTokenSource`] over the managed and workload identity providers in
//! [`crate::origin::azure_auth`].
//!
//! [`AzureTokenCaches`] is node-wide: one [`AzureTokenCache`] per distinct
//! credential (kind, client ID, tenant ID), created on first use so a node
//! without credentialed caches never reads token settings. [`AzureTokens`] is
//! the worker-local view holding one fetching source per credential.
//!
//! Storage 401s feed back through [`AzureTokenSource::rejected`]: a
//! challenge teaches workload identity its tenant, and a rejected token is
//! dropped so the retry fetches a fresh one. Token drops are rate limited per
//! worker and credential so a persistent 401 cannot hammer the token service.

use super::{AzureTokenSource, BearerToken};
use crate::error::{Error, Operation, Result};
use crate::origin::azure_auth::{self, AuthEndpoints, AuthError, AzureTokenCache};
use crate::remote::Resolver;
use crate::runtime::{Reactor, RequestScope};
use racer_control_wire::AzureCredential;
use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// Shortest interval between token drops for one credential on one worker.
const INVALIDATE_INTERVAL: Duration = Duration::from_secs(10);

/// Token cache identity: (kind, client ID, tenant ID).
type Key = (u8, Option<String>, Option<String>);

/// The token provider credential and cache key, or `None` for anonymous access.
fn provider(credential: &AzureCredential) -> Option<(Key, azure_auth::AzureCredential)> {
    match credential {
        AzureCredential::Anonymous => None,
        AzureCredential::ManagedIdentity { client_id } => Some((
            (1, client_id.clone(), None),
            azure_auth::AzureCredential::ManagedIdentity {
                client_id: client_id.clone(),
            },
        )),
        AzureCredential::WorkloadIdentity {
            client_id,
            tenant_id,
        } => Some((
            (2, Some(client_id.clone()), tenant_id.clone()),
            azure_auth::AzureCredential::WorkloadIdentity {
                client_id: client_id.clone(),
                tenant_id: tenant_id.clone(),
            },
        )),
    }
}

/// Node-wide token caches, shared by every worker.
pub struct AzureTokenCaches {
    endpoints: AuthEndpoints,
    caches: Mutex<HashMap<Key, Arc<AzureTokenCache>>>,
}

impl AzureTokenCaches {
    /// Production token endpoints from the environment.
    pub fn from_env() -> Self {
        Self::new(AuthEndpoints::from_env())
    }

    pub fn new(endpoints: AuthEndpoints) -> Self {
        Self {
            endpoints,
            caches: Mutex::default(),
        }
    }

    fn cache(
        &self,
        key: &Key,
        credential: azure_auth::AzureCredential,
    ) -> Result<Arc<AzureTokenCache>> {
        let mut caches = self.caches.lock().map_err(|_| Error::Internal)?;
        if let Some(cache) = caches.get(key) {
            return Ok(cache.clone());
        }
        let cache = Arc::new(AzureTokenCache::new(credential, self.endpoints.clone())?);
        caches.insert(key.clone(), cache.clone());
        Ok(cache)
    }
}

impl std::fmt::Debug for AzureTokenCaches {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("AzureTokenCaches")
    }
}

struct Source {
    source: Rc<azure_auth::AzureTokenSource>,
    invalidated: Option<Instant>,
}

/// Worker-local token sources.
pub struct AzureTokens {
    caches: Arc<AzureTokenCaches>,
    reactor: Rc<Reactor>,
    resolver: Option<Rc<dyn Resolver>>,
    sources: RefCell<HashMap<Key, Source>>,
}

impl AzureTokens {
    /// Fetch on `reactor` with DNS resolution.
    pub fn new(caches: Arc<AzureTokenCaches>, reactor: Rc<Reactor>) -> Self {
        Self {
            caches,
            reactor,
            resolver: None,
            sources: RefCell::default(),
        }
    }

    /// Fetch with an explicit resolver (tests).
    pub fn with_resolver(mut self, resolver: Rc<dyn Resolver>) -> Self {
        self.resolver = Some(resolver);
        self
    }

    fn source(
        &self,
        key: &Key,
        credential: azure_auth::AzureCredential,
    ) -> Result<Rc<azure_auth::AzureTokenSource>> {
        if let Some(source) = self.sources.borrow().get(key) {
            return Ok(source.source.clone());
        }
        let cache = self.caches.cache(key, credential)?;
        let source = Rc::new(match &self.resolver {
            Some(resolver) => azure_auth::AzureTokenSource::with_resolver(
                cache,
                self.reactor.clone(),
                resolver.clone(),
            ),
            None => azure_auth::AzureTokenSource::new(cache, self.reactor.clone()),
        });
        self.sources.borrow_mut().insert(
            key.clone(),
            Source {
                source: source.clone(),
                invalidated: None,
            },
        );
        Ok(source)
    }
}

impl AzureTokenSource for AzureTokens {
    fn bearer<'a>(
        &'a self,
        credential: &'a AzureCredential,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<BearerToken>> {
        Box::pin(async move {
            let Some((key, provider)) = provider(credential) else {
                return Ok(None);
            };
            let source = self.source(&key, provider)?;
            match source.bearer(scope).await {
                Ok(token) => BearerToken::new(token.as_bytes().to_vec()).map(Some),
                // Send anonymously; the 401 challenge names the tenant.
                Err(AuthError::TenantUnknown) => Ok(None),
                Err(error) => Err(error.into()),
            }
        })
    }

    fn rejected(&self, credential: &AzureCredential, challenge: Option<&[u8]>, sent: bool) -> bool {
        let Some((key, provider)) = provider(credential) else {
            return false;
        };
        let Ok(source) = self.source(&key, provider) else {
            return false;
        };
        let cache = source.cache();
        if !sent {
            // Only workload identity without a tenant sends anonymously.
            return challenge.is_some_and(|challenge| cache.learn_tenant(challenge));
        }
        if let Some(challenge) = challenge {
            cache.learn_tenant(challenge);
        }
        let now = Instant::now();
        let mut sources = self.sources.borrow_mut();
        let Some(entry) = sources.get_mut(&key) else {
            return false;
        };
        if entry
            .invalidated
            .is_some_and(|at| now < at + INVALIDATE_INTERVAL)
        {
            return false;
        }
        entry.invalidated = Some(now);
        cache.invalidate();
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn credentials_map_to_distinct_caches() {
        assert!(provider(&AzureCredential::Anonymous).is_none());
        let managed = provider(&AzureCredential::ManagedIdentity { client_id: None }).unwrap();
        let assigned = provider(&AzureCredential::ManagedIdentity {
            client_id: Some("c".into()),
        })
        .unwrap();
        let workload = provider(&AzureCredential::WorkloadIdentity {
            client_id: "c".into(),
            tenant_id: None,
        })
        .unwrap();
        let tenant = provider(&AzureCredential::WorkloadIdentity {
            client_id: "c".into(),
            tenant_id: Some("t".into()),
        })
        .unwrap();
        let keys = [&managed.0, &assigned.0, &workload.0, &tenant.0];
        for (i, a) in keys.iter().enumerate() {
            for b in &keys[i + 1..] {
                assert_ne!(a, b);
            }
        }
        assert_eq!(
            tenant.1,
            azure_auth::AzureCredential::WorkloadIdentity {
                client_id: "c".into(),
                tenant_id: Some("t".into())
            }
        );
    }
}
