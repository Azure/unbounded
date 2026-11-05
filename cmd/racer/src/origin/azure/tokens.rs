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
//! dropped so the retry fetches a fresh one. Only the token that was sent is
//! dropped: when another request or worker already replaced it, the retry
//! uses the newer token and nothing is evicted. Token drops are rate limited
//! per worker and credential so a persistent 401 cannot hammer the token
//! service.

use super::{AzureTokenSource, BearerToken};
use crate::error::{Error, Operation, Result};
use crate::origin::azure_auth::{self, AuthEndpoints, AuthError, AzureTokenCache};
use crate::remote::Resolver;
use crate::runtime::{Reactor, RequestScope};
use racer_wire::control::AzureCredential;
use racer_wire::control::OriginDefinition;
use std::cell::RefCell;
use std::collections::{HashMap, HashSet};
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
    /// Weak, so a credential no worker uses any more drops its cache (and its
    /// tokens). Workers own the caches through their sources.
    caches: Mutex<HashMap<Key, std::sync::Weak<AzureTokenCache>>>,
}

impl AzureTokenCaches {
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
        if let Some(cache) = caches.get(key).and_then(std::sync::Weak::upgrade) {
            return Ok(cache);
        }
        // New credentials are rare, so sweeping here keeps the map at the
        // credentials some worker still holds.
        caches.retain(|_, cache| cache.strong_count() > 0);
        let cache = Arc::new(AzureTokenCache::new(credential, self.endpoints.clone())?);
        caches.insert(key.clone(), Arc::downgrade(&cache));
        Ok(cache)
    }

    /// Credentials with a cache entry, live or not yet swept (tests).
    #[cfg(test)]
    pub(crate) fn tracked(&self) -> usize {
        self.caches.lock().map_or(0, |caches| caches.len())
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
    /// Worker-local sources (tests).
    #[cfg(test)]
    pub(crate) fn source_count(&self) -> usize {
        self.sources.borrow().len()
    }

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

    fn rejected(
        &self,
        credential: &AzureCredential,
        challenge: Option<&[u8]>,
        sent: Option<&[u8]>,
    ) -> bool {
        self.rejected_at(credential, challenge, sent, self.reactor.ctx().clock.now())
    }

    /// Drop sources for credentials no published origin uses. A request in
    /// flight keeps its own `Rc` to the source.
    fn retain(&self, live: &[&OriginDefinition]) {
        let live: HashSet<Key> = live
            .iter()
            .filter_map(|origin| provider(&origin.azure.credential).map(|(key, _)| key))
            .collect();
        let mut sources = self.sources.borrow_mut();
        let stale: Vec<_> = sources.extract_if(|key, _| !live.contains(key)).collect();
        drop(sources);
        drop(stale);
    }
}

impl AzureTokens {
    /// [`AzureTokenSource::rejected`] at `now`, so tests can step the
    /// invalidation window without sleeping.
    fn rejected_at(
        &self,
        credential: &AzureCredential,
        challenge: Option<&[u8]>,
        sent: Option<&[u8]>,
        now: Instant,
    ) -> bool {
        let Some((key, provider)) = provider(credential) else {
            return false;
        };
        let Ok(source) = self.source(&key, provider) else {
            return false;
        };
        let cache = source.cache();
        let Some(sent) = sent else {
            // Only workload identity without a tenant sends anonymously.
            return challenge.is_some_and(|challenge| cache.learn_tenant(challenge));
        };
        if let Some(challenge) = challenge {
            cache.learn_tenant(challenge);
        }
        if !cache.holds(sent) {
            // Already dropped or replaced (by a tenant change, another
            // request, or another worker): retry with the current token
            // without spending this worker's invalidation window.
            return true;
        }
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
        // Compare and drop under the cache lock: a newer token stored since
        // the check above survives.
        cache.invalidate_sent(sent);
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::remote::RemoteEndpoint;
    use crate::remote::tests::{MapResolver, serve};
    use crate::test_support::enrollment::{drive, reactor, scope};
    use std::sync::atomic::Ordering;

    fn managed(client_id: &str) -> AzureCredential {
        AzureCredential::ManagedIdentity {
            client_id: Some(client_id.into()),
        }
    }

    fn bearer(tokens: &AzureTokens, credential: &AzureCredential) -> String {
        let scope = scope();
        let token = drive(
            &tokens.reactor,
            Box::pin(async { Ok(tokens.bearer(credential, &scope).await) }),
        )
        .unwrap()
        .unwrap()
        .unwrap();
        String::from_utf8(token.as_bytes().to_vec()).unwrap()
    }

    #[test]
    fn rejected_tokens_refresh_at_most_once_per_window_per_credential() {
        // IMDS names each token after the requesting client and request count.
        let count = Arc::new(Mutex::new(0usize));
        let seen = count.clone();
        let (address, accepted, server) = serve(None, 6, move |head, _| {
            let mut count = seen.lock().unwrap();
            *count += 1;
            let client = if head.contains("client_id=a ") {
                "a"
            } else {
                "b"
            };
            let body = format!(
                "{{\"access_token\":\"{client}-{count}\",\"expires_in\":\"3600\",\"token_type\":\"Bearer\"}}"
            );
            format!(
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{body}",
                body.len()
            )
            .into_bytes()
        });
        let Some(reactor) = reactor() else {
            let _ = std::net::TcpStream::connect(address);
            return;
        };
        let caches = Arc::new(AzureTokenCaches::new(AuthEndpoints {
            imds: RemoteEndpoint::http("127.0.0.1", address.port()).unwrap(),
            authority: "https://login.microsoftonline.com/".into(),
            federated_token_file: None,
            tls: None,
        }));
        let tokens =
            AzureTokens::new(caches, reactor).with_resolver(Rc::new(MapResolver::new(Vec::new())));
        let (a, b) = (managed("a"), managed("b"));

        assert!(!tokens.rejected(&AzureCredential::Anonymous, None, Some(b"x")));
        assert_eq!(bearer(&tokens, &a), "a-1");
        assert_eq!(bearer(&tokens, &b), "b-2");

        // A rejected token is dropped and the next bearer fetches a new one.
        let start = Instant::now();
        assert!(tokens.rejected_at(&a, None, Some(b"a-1"), start));
        assert_eq!(bearer(&tokens, &a), "a-3");
        // Other credentials keep their tokens.
        assert_eq!(bearer(&tokens, &b), "b-2");

        // Within the window a second rejection keeps the token.
        let within = start + INVALIDATE_INTERVAL - Duration::from_millis(1);
        assert!(!tokens.rejected_at(&a, None, Some(b"a-3"), within));
        assert_eq!(bearer(&tokens, &a), "a-3");
        // The window is per credential.
        assert!(tokens.rejected_at(&b, None, Some(b"b-2"), within));
        assert_eq!(bearer(&tokens, &b), "b-4");
        assert_eq!(bearer(&tokens, &a), "a-3");

        // Once the window passes the rejected token is dropped again.
        assert!(tokens.rejected_at(&a, None, Some(b"a-3"), start + INVALIDATE_INTERVAL));
        assert_eq!(bearer(&tokens, &a), "a-5");
        assert_eq!(bearer(&tokens, &b), "b-4");
        assert_eq!(accepted.load(Ordering::SeqCst), 5);

        drop(tokens);
        // Unblock the server's remaining accept.
        let _ = std::net::TcpStream::connect(address);
        server.join().unwrap();
    }

    #[test]
    fn stale_rejections_keep_newer_tokens_across_workers() {
        let count = Arc::new(Mutex::new(0usize));
        let seen = count.clone();
        let (address, accepted, server) = serve(None, 4, move |_, _| {
            let mut count = seen.lock().unwrap();
            *count += 1;
            let body = format!(
                "{{\"access_token\":\"t-{count}\",\"expires_in\":\"3600\",\"token_type\":\"Bearer\"}}"
            );
            format!(
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{body}",
                body.len()
            )
            .into_bytes()
        });
        let Some(reactor) = reactor() else {
            let _ = std::net::TcpStream::connect(address);
            return;
        };
        let caches = Arc::new(AzureTokenCaches::new(AuthEndpoints {
            imds: RemoteEndpoint::http("127.0.0.1", address.port()).unwrap(),
            authority: "https://login.microsoftonline.com/".into(),
            federated_token_file: None,
            tls: None,
        }));
        // Two workers sharing the node-wide cache.
        let one = AzureTokens::new(caches.clone(), reactor.clone())
            .with_resolver(Rc::new(MapResolver::new(Vec::new())));
        let two =
            AzureTokens::new(caches, reactor).with_resolver(Rc::new(MapResolver::new(Vec::new())));
        let a = managed("a");
        assert_eq!(bearer(&one, &a), "t-1");
        assert_eq!(bearer(&two, &a), "t-1");

        // Worker one drops t-1 and refreshes.
        let start = Instant::now();
        assert!(one.rejected_at(&a, None, Some(b"t-1"), start));
        assert_eq!(bearer(&one, &a), "t-2");

        // A late 401 for t-1 on worker two retries with t-2 and keeps it.
        assert!(two.rejected_at(&a, None, Some(b"t-1"), start));
        assert_eq!(bearer(&two, &a), "t-2");
        assert_eq!(bearer(&one, &a), "t-2");

        // That stale rejection did not spend worker two's window: a
        // rejection of the current token still drops it.
        assert!(two.rejected_at(&a, None, Some(b"t-2"), start));
        assert_eq!(bearer(&one, &a), "t-3");
        // Worker two's window is now spent for the current token.
        assert!(!two.rejected_at(&a, None, Some(b"t-3"), start));
        // A stale rejection on the same worker within its own window still
        // retries, since the current token differs.
        assert!(one.rejected_at(&a, None, Some(b"t-1"), start));
        assert_eq!(bearer(&two, &a), "t-3");
        // An unknown token is never evicted.
        assert!(one.rejected_at(&a, None, Some(b"other"), start));
        assert_eq!(bearer(&one, &a), "t-3");
        assert_eq!(accepted.load(Ordering::SeqCst), 3);

        drop((one, two));
        let _ = std::net::TcpStream::connect(address);
        server.join().unwrap();
    }

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

    #[test]
    fn sources_and_caches_for_unpublished_credentials_are_dropped() {
        let Some(reactor) = reactor() else { return };
        let caches = Arc::new(AzureTokenCaches::new(AuthEndpoints {
            imds: RemoteEndpoint::http("127.0.0.1", 9).unwrap(),
            authority: "https://login.microsoftonline.com/".into(),
            federated_token_file: None,
            tls: None,
        }));
        let tokens = AzureTokens::new(caches.clone(), reactor);
        let origin = |credential: AzureCredential| {
            OriginDefinition::azure(
                racer_wire::control::AzureContainerUrl::parse(
                    "https://acct.blob.core.windows.net/models",
                )
                .unwrap(),
                credential,
            )
        };
        let (a, b) = (origin(managed("a")), origin(managed("b")));
        let source = |definition: &OriginDefinition| {
            let (key, provider) = provider(&definition.azure.credential).unwrap();
            tokens.source(&key, provider).unwrap()
        };
        source(&a);
        // A request in flight holds its source, and with it the cache.
        let held = source(&b);
        assert_eq!((tokens.source_count(), caches.tracked()), (2, 2));

        // b's credential is edited away: only a's source stays on this worker.
        tokens.retain(&[&a, &origin(AzureCredential::Anonymous)]);
        assert_eq!(tokens.source_count(), 1);
        assert_eq!(Arc::strong_count(held.cache()), 1);
        drop(held);
        // The node-wide entry for b is swept when the next credential arrives.
        source(&origin(managed("c")));
        assert_eq!((tokens.source_count(), caches.tracked()), (2, 2));
        tokens.retain(&[]);
        assert_eq!(tokens.source_count(), 0);
        source(&a);
        assert_eq!(caches.tracked(), 1);
    }
}
