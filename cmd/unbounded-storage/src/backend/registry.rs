// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shard-local, live-reloadable registry of origin backends.
//!
//! A shard no longer fetches from a single origin tier. Every request
//! already names the backend it targets through
//! [`OriginRef::backend_id`](crate::storage::OriginRef), stamped by the
//! frontend that accepted it, so the data path can resolve which
//! configured backend to fetch from per request. [`BackendRegistry`]
//! owns the `name -> OriginBackend` map behind an [`ArcSwap`] so the
//! config watcher can add, remove, or replace a backend at runtime
//! without tearing down the shard: a reconcile pass builds the new map
//! and publishes it atomically, and the next fetch observes it.
//!
//! The registry implements [`Backend`] itself, so it drops into the
//! same generic slot the single [`OriginBackend`] used to fill in the
//! pool transport and the recursive RPC handler. It also implements
//! [`BackendReconcileTarget`] so the live-config reconciler drives it
//! the same way it drives the peer fabric.

use std::collections::HashMap;
use std::io;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::{Context, Poll};

use arc_swap::ArcSwap;

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::config::reconcile::BackendReconcileTarget;
use crate::config::{BackendSpec, backend_spec};
use crate::storage::StripeReq;
use crate::tls::{TlsConfig, TlsContext};

use super::{
    AzureBackend, Backend, FakeBackend, HttpBackend, OriginBackend, OriginRing, OriginStream,
    S3Backend,
};
use super::url::parse_endpoint;

/// The build context a registry needs to (re)construct an
/// [`OriginBackend`] from a [`BackendSpec`]. Captured once per registry
/// instance because the pool transport and the RPC handler each own a
/// registry that differs only in which ring a cache-miss fetch drives
/// and which registered region origin bytes land in.
#[derive(Clone)]
struct BuildCtx {
    ring: OriginRing,
    page_size: usize,
    backing_base: *mut u8,
}

/// A shard-local set of origin backends keyed by component name, swappable at
/// runtime. Clones share the same underlying map (cheap `Arc` clone),
/// so a clone handed to the control-drain tick hook reconciles the
/// very map the data path reads.
#[derive(Clone)]
pub struct BackendRegistry {
    backends: Arc<ArcSwap<HashMap<String, Arc<OriginBackend>>>>,
    ctx: BuildCtx,
}

// SAFETY: shard-pinned exactly like the `OriginBackend`s it holds (see
// `backend::http`/`backend::s3`). The `*mut u8` build base and the
// `Rc`-bearing `OriginRing::Shard` are only ever touched on the owning
// shard thread or the ephemeral `fabric-rpc-worker` thread driving a
// `WorkerLocal` ring; the marker only lets the registry live in the
// `Send + Sync` generic slots the transport/handler require. It is not
// safe to move a registry to an unrelated thread.
unsafe impl Send for BackendRegistry {}
unsafe impl Sync for BackendRegistry {}

impl BackendRegistry {
    /// Build a registry seeded from `specs`, fetching through `ring`
    /// into the region based at `backing_base`. A spec that fails to
    /// build (e.g. a URL that does not resolve) aborts the seed
    /// with the error, because startup must fail loudly rather than
    /// silently drop a configured backend.
    pub fn new(
        specs: &[BackendSpec],
        ring: OriginRing,
        page_size: usize,
        backing_base: *mut u8,
    ) -> io::Result<Self> {
        let ctx = BuildCtx {
            ring,
            page_size,
            backing_base,
        };
        let mut map: HashMap<String, Arc<OriginBackend>> = HashMap::with_capacity(specs.len());
        for spec in specs {
            map.insert(spec.name.clone(), Arc::new(ctx.build(spec)?));
        }
        Ok(Self {
            backends: Arc::new(ArcSwap::from_pointee(map)),
            ctx,
        })
    }

    /// Number of backends currently registered.
    #[cfg(test)]
    pub fn len(&self) -> usize {
        self.backends.load().len()
    }

    /// Whether a backend with `name` is currently registered.
    #[cfg(test)]
    pub fn contains(&self, id: &str) -> bool {
        self.backends.load().contains_key(id)
    }

    /// Copy-on-write mutate the live map: clone the current contents,
    /// apply `f`, then publish the result atomically.
    fn mutate<F: FnOnce(&mut HashMap<String, Arc<OriginBackend>>)>(&self, f: F) {
        let mut next = HashMap::clone(&self.backends.load());
        f(&mut next);
        self.backends.store(Arc::new(next));
    }
}

impl BuildCtx {
    /// Build one [`OriginBackend`] from `spec`, selecting the concrete
    /// origin implementation by [`BackendSpec::config`]. Real origin URLs
    /// are parsed as `scheme://host[:port]` values. The authority
    /// (`host:port`, port defaulted from the scheme) is resolved to an origin
    /// address. The `Host:` header carries the host with any non-default port;
    /// the bare host is used for TLS SNI/certificate verification. For an
    /// `https` URL a shared [`TlsContext`] is built from the backend's TLS knobs
    /// and handed to the backend; `http` URLs pass `None` and stay plaintext.
    fn build(&self, spec: &BackendSpec) -> io::Result<OriginBackend> {
        match spec.config.as_ref() {
            Some(backend_spec::Config::Http(cfg)) => {
                let endpoint = build_origin_endpoint(
                    &cfg.url,
                    &cfg.ca_cert_path,
                    cfg.insecure_skip_verify,
                )?;
                let origin = HttpBackend::resolve_origin(&endpoint.authority)?;
                Ok(OriginBackend::Http(HttpBackend::new(
                    self.ring.clone(),
                    origin,
                    endpoint.host,
                    endpoint.sni_host,
                    endpoint.tls,
                    spec.name.clone(),
                    cfg.stripe_size_bytes.expect("stripe_size_bytes defaulted"),
                    self.page_size,
                    self.backing_base,
                    cfg.http_concurrency.expect("http_concurrency defaulted") as usize,
                )))
            }
            Some(backend_spec::Config::S3(cfg)) => {
                let endpoint = build_origin_endpoint(
                    &cfg.url,
                    &cfg.ca_cert_path,
                    cfg.insecure_skip_verify,
                )?;
                let origin = S3Backend::resolve_origin(&endpoint.authority)?;
                Ok(OriginBackend::S3(S3Backend::new(
                    self.ring.clone(),
                    origin,
                    endpoint.host,
                    endpoint.sni_host,
                    endpoint.tls,
                    spec.name.clone(),
                    cfg.stripe_size_bytes.expect("stripe_size_bytes defaulted"),
                    self.page_size,
                    self.backing_base,
                    cfg.http_concurrency.expect("http_concurrency defaulted") as usize,
                )))
            }
            Some(backend_spec::Config::Azure(cfg)) => {
                let endpoint = build_origin_endpoint(
                    &cfg.url,
                    &cfg.ca_cert_path,
                    cfg.insecure_skip_verify,
                )?;
                let origin = AzureBackend::resolve_origin(&endpoint.authority)?;
                Ok(OriginBackend::Azure(AzureBackend::new(
                    self.ring.clone(),
                    origin,
                    endpoint.host,
                    endpoint.sni_host,
                    endpoint.tls,
                    spec.name.clone(),
                    cfg.stripe_size_bytes.expect("stripe_size_bytes defaulted"),
                    self.page_size,
                    self.backing_base,
                    cfg.http_concurrency.expect("http_concurrency defaulted") as usize,
                )))
            }
            Some(backend_spec::Config::Fake(cfg)) => Ok(OriginBackend::Fake(FakeBackend::new(
                spec.name.clone(),
                cfg.object_size_bytes.expect("object_size_bytes defaulted"),
                self.page_size,
                self.backing_base,
            ))),
            None => Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("backend {} missing config", spec.name),
            )),
        }
    }
}

struct OriginEndpoint {
    authority: String,
    host: String,
    sni_host: String,
    tls: Option<Rc<TlsContext>>,
}

fn build_origin_endpoint(
    url: &str,
    ca_cert_path: &Option<String>,
    insecure_skip_verify: bool,
) -> io::Result<OriginEndpoint> {
    let url = parse_endpoint(url)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e.to_string()))?;
    let authority = url.authority();
    let tls = if url.scheme.is_tls() {
        let cfg = TlsConfig {
            ca_cert_path: ca_cert_path.clone(),
            insecure_skip_verify,
        };
        let ctx = TlsContext::new(&cfg).map_err(|e| io::Error::other(e.to_string()))?;
        Some(Rc::new(ctx))
    } else {
        None
    };

    Ok(OriginEndpoint {
        authority,
        host: url.host_header(),
        sni_host: url.host,
        tls,
    })
}

impl Backend for BackendRegistry {
    type Req = StripeReq;
    type Stream<'a> = RegistryFetchStream;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        let backend_id = match req.origin() {
            Some(origin) => origin.backend_id.as_str(),
            None => return RegistryFetchStream::unknown(""),
        };
        let map = self.backends.load();
        match map.get(backend_id) {
            // `fetch_stream` returns a fully owned `'static` stream that
            // borrows nothing from the backend, so the `Arc` guard can
            // be dropped the moment the call returns.
            Some(backend) => RegistryFetchStream::Origin(backend.fetch_stream(req, src, dsts)),
            None => RegistryFetchStream::unknown(backend_id),
        }
    }
}

impl BackendReconcileTarget for BackendRegistry {
    fn list(&self) -> Vec<String> {
        self.backends.load().keys().cloned().collect()
    }

    fn add(&self, spec: &BackendSpec) -> Result<(), String> {
        // Build before taking the swap so a failed build leaves the
        // live map untouched.
        let backend = self
            .ctx
            .build(spec)
            .map_err(|e| format!("build backend {}: {e}", spec.name))?;
        let built = Arc::new(backend);
        self.mutate(|map| {
            map.insert(spec.name.clone(), built);
        });
        Ok(())
    }

    fn remove(&self, id: &str) -> Result<(), String> {
        self.mutate(|map| {
            map.remove(id);
        });
        Ok(())
    }
}

/// Stream produced by [`BackendRegistry::bulk_get`]: either the chosen
/// backend's owned [`OriginStream`], or a one-shot error for a request
/// naming a backend the registry does not hold. The unknown-backend
/// case should not arise for a validated config (load-time validation
/// guarantees every frontend's backend exists), but a request can race
/// a `remove`, so it is surfaced as a transport error rather than a
/// panic.
pub enum RegistryFetchStream {
    Origin(OriginStream<'static>),
    Unknown(Option<Error>),
}

impl RegistryFetchStream {
    fn unknown(backend_id: &str) -> Self {
        let msg = format!("unknown backend id: {backend_id:?}");
        let err = Error::transport(io::Error::new(io::ErrorKind::NotFound, msg));
        RegistryFetchStream::Unknown(Some(err))
    }
}

impl PageStream for RegistryFetchStream {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        match self.get_mut() {
            // `OriginStream` is `Unpin` (its only pinned state is a
            // `Pin<Box<dyn Future>>`), so re-pinning in place is sound.
            RegistryFetchStream::Origin(s) => Pin::new(s).poll_next(cx),
            RegistryFetchStream::Unknown(err) => match err.take() {
                Some(e) => Poll::Ready(Some(Err(e))),
                None => Poll::Ready(None),
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use std::ptr;
    use std::task::{RawWaker, RawWakerVTable, Waker};

    use crate::config::{FakeBackendConfig, HttpBackendConfig, backend_spec};

    use super::*;

    fn http_spec(id: &str, url: &str) -> BackendSpec {
        BackendSpec {
            name: id.to_string(),
            config: Some(backend_spec::Config::Http(HttpBackendConfig {
                url: url.to_string(),
                stripe_size_bytes: Some(4 * 1024 * 1024),
                http_concurrency: Some(64),
                ca_cert_path: None,
                insecure_skip_verify: false,
            })),
        }
    }

    fn fake_spec(id: &str, object_size: u64) -> BackendSpec {
        BackendSpec {
            name: id.to_string(),
            config: Some(backend_spec::Config::Fake(FakeBackendConfig {
                stripe_size_bytes: Some(4 * 1024 * 1024),
                object_size_bytes: Some(object_size),
            })),
        }
    }

    /// A registry over a worker-local ring with no fixed region and a
    /// null backing base. Sound for tests that never drive a fetch (they
    /// only exercise the id map and the unknown-backend path); the ring
    /// is built lazily on first `handle()`, which these tests never hit.
    fn registry(specs: &[BackendSpec]) -> BackendRegistry {
        BackendRegistry::new(
            specs,
            OriginRing::WorkerLocal {
                queue_depth: 1,
                region: None,
            },
            4096,
            ptr::null_mut(),
        )
        .expect("seed registry")
    }

    fn noop_waker() -> Waker {
        fn no_op(_: *const ()) {}
        fn clone(_: *const ()) -> RawWaker {
            RawWaker::new(ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, no_op, no_op, no_op);
        // SAFETY: the vtable's fns are all no-ops over a null data
        // pointer, so the waker upholds the `RawWaker` contract.
        unsafe { Waker::from_raw(RawWaker::new(ptr::null(), &VTABLE)) }
    }

    #[test]
    fn seeds_and_lists_configured_backends() {
        let reg = registry(&[
            http_spec("a", "http://127.0.0.1:1"),
            http_spec("b", "http://127.0.0.1:2"),
        ]);
        assert_eq!(reg.len(), 2);
        assert!(reg.contains("a"));
        assert!(reg.contains("b"));
        let mut ids = reg.list();
        ids.sort();
        assert_eq!(ids, vec!["a".to_string(), "b".to_string()]);
    }

    #[test]
    fn add_then_remove_mutates_live_map() {
        let reg = registry(&[]);
        assert_eq!(reg.len(), 0);

        reg.add(&http_spec("a", "http://127.0.0.1:1"))
            .expect("add a");
        assert!(reg.contains("a"));
        assert_eq!(reg.len(), 1);

        // Re-adding the same id replaces in place rather than growing.
        reg.add(&http_spec("a", "http://127.0.0.1:9"))
            .expect("replace a");
        assert_eq!(reg.len(), 1);

        reg.remove("a").expect("remove a");
        assert!(!reg.contains("a"));
        assert_eq!(reg.len(), 0);
    }

    #[test]
    fn add_with_unresolvable_url_leaves_map_untouched() {
        let reg = registry(&[http_spec("a", "http://127.0.0.1:1")]);
        let err = reg
            .add(&http_spec("b", "this is not a valid url"))
            .expect_err("bad url should fail to build");
        assert!(err.contains("build backend b"), "unexpected error: {err}");
        // The failed build must not have disturbed the live map.
        assert_eq!(reg.len(), 1);
        assert!(reg.contains("a"));
        assert!(!reg.contains("b"));
    }

    #[test]
    fn unknown_backend_stream_yields_one_error_then_ends() {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        let mut stream = RegistryFetchStream::unknown("missing");
        let first = Pin::new(&mut stream).poll_next(&mut cx);
        assert!(
            matches!(first, Poll::Ready(Some(Err(_)))),
            "expected an immediate error for an unknown backend",
        );
        let second = Pin::new(&mut stream).poll_next(&mut cx);
        assert!(
            matches!(second, Poll::Ready(None)),
            "expected end-of-stream after the error",
        );
    }

    #[test]
    fn seeds_a_fake_backend_with_no_url() {
        // A fake backend needs no URL and no ring, so it must build
        // even against the null backing base these tests use.
        let reg = registry(&[fake_spec("synthetic", 1024 * 1024)]);
        assert_eq!(reg.len(), 1);
        assert!(reg.contains("synthetic"));
    }
}
