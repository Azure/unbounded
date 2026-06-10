// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shard-local, live-reloadable registry of origin backends.
//!
//! A shard no longer fetches from a single origin tier. Every request
//! already names the backend it targets through
//! [`OriginRef::backend_id`](crate::storage::OriginRef), stamped by the
//! frontend that accepted it, so the data path can resolve which
//! configured backend to fetch from per request. [`BackendRegistry`]
//! owns the `id -> OriginBackend` map behind an [`ArcSwap`] so the
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
use std::sync::Arc;
use std::task::{Context, Poll};

use arc_swap::ArcSwap;

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::config::reconcile::BackendReconcileTarget;
use crate::config::{BackendKind, BackendSpec};
use crate::storage::StripeReq;

use super::{Backend, AzureBackend, HttpBackend, OriginBackend, OriginRing, OriginStream, S3Backend};

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

/// A shard-local set of origin backends keyed by `id`, swappable at
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
    /// build (e.g. an endpoint that does not resolve) aborts the seed
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
            map.insert(spec.id.clone(), Arc::new(ctx.build(spec)?));
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

    /// Whether a backend with `id` is currently registered.
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
    /// origin implementation by [`BackendSpec::kind`]. For an `s3`
    /// backend the origin IP is resolved from the endpoint the same way
    /// as for HTTP (IPv4-only, v1), but the host authority is extracted
    /// separately for the `Host:` header.
    fn build(&self, spec: &BackendSpec) -> io::Result<OriginBackend> {
        match spec.kind() {
            BackendKind::Http => {
                let origin = HttpBackend::resolve_origin(&spec.endpoint)?;
                Ok(OriginBackend::Http(HttpBackend::new(
                    self.ring.clone(),
                    origin,
                    spec.id.clone(),
                    spec.stripe_size_bytes,
                    self.page_size,
                    self.backing_base,
                    spec.http_concurrency as usize,
                )))
            }
            BackendKind::S3 => {
                let origin = S3Backend::resolve_origin(&spec.endpoint)?;
                let host = extract_host_authority(&spec.endpoint);
                Ok(OriginBackend::S3(S3Backend::new(
                    self.ring.clone(),
                    origin,
                    host,
                    spec.id.clone(),
                    spec.stripe_size_bytes,
                    self.page_size,
                    self.backing_base,
                    spec.http_concurrency as usize,
                )))
            }
            BackendKind::Azure => {
                let origin = AzureBackend::resolve_origin(&spec.endpoint)?;
                let host = extract_host_authority(&spec.endpoint);
                Ok(OriginBackend::Azure(AzureBackend::new(
                    self.ring.clone(),
                    origin,
                    host,
                    spec.id.clone(),
                    spec.stripe_size_bytes,
                    self.page_size,
                    self.backing_base,
                    spec.http_concurrency as usize,
                )))
            }
        }
    }
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
            .map_err(|e| format!("build backend {}: {e}", spec.id))?;
        let built = Arc::new(backend);
        self.mutate(|map| {
            map.insert(spec.id.clone(), built);
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

/// Extract the `host[:port]` authority from a backend `endpoint` for
/// use as the S3 `Host:` header.
///
/// Strips an optional `scheme://` prefix and any `/path` or `?query`
/// suffix, but preserves the port so the `Host:` header stays
/// RFC 7230 conformant for custom-port origins (e.g. MinIO on `:9000`).
fn extract_host_authority(endpoint: &str) -> String {
    let after_scheme = match endpoint.split_once("://") {
        Some((_, rest)) => rest,
        None => endpoint,
    };
    after_scheme
        .split(['/', '?'])
        .next()
        .unwrap_or(after_scheme)
        .to_string()
}

#[cfg(test)]
mod tests {
    use std::ptr;
    use std::task::{RawWaker, RawWakerVTable, Waker};

    use crate::config::BackendKind;

    use super::*;

    fn http_spec(id: &str, endpoint: &str) -> BackendSpec {
        BackendSpec {
            id: id.to_string(),
            kind: BackendKind::Http as i32,
            endpoint: endpoint.to_string(),
            stripe_size_bytes: 4 * 1024 * 1024,
            http_concurrency: 64,
            bucket: None,
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
        let reg = registry(&[http_spec("a", "127.0.0.1:1"), http_spec("b", "127.0.0.1:2")]);
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

        reg.add(&http_spec("a", "127.0.0.1:1")).expect("add a");
        assert!(reg.contains("a"));
        assert_eq!(reg.len(), 1);

        // Re-adding the same id replaces in place rather than growing.
        reg.add(&http_spec("a", "127.0.0.1:9")).expect("replace a");
        assert_eq!(reg.len(), 1);

        reg.remove("a").expect("remove a");
        assert!(!reg.contains("a"));
        assert_eq!(reg.len(), 0);
    }

    #[test]
    fn add_with_unresolvable_endpoint_leaves_map_untouched() {
        let reg = registry(&[http_spec("a", "127.0.0.1:1")]);
        let err = reg
            .add(&http_spec("b", "this is not a valid endpoint"))
            .expect_err("bad endpoint should fail to build");
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
    fn extract_host_authority_preserves_port_strips_scheme_and_path() {
        assert_eq!(
            extract_host_authority("s3.example.com:443"),
            "s3.example.com:443"
        );
        assert_eq!(
            extract_host_authority("https://s3.us-east-1.amazonaws.com:443"),
            "s3.us-east-1.amazonaws.com:443"
        );
        assert_eq!(extract_host_authority("http://origin/path"), "origin");
        assert_eq!(
            extract_host_authority("origin.example.com"),
            "origin.example.com"
        );
        assert_eq!(extract_host_authority("127.0.0.1:9000"), "127.0.0.1:9000");
    }
}
