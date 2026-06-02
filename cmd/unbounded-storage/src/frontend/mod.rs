// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(async_fn_in_trait)]

//! Client-facing tier (an HTTP server) that streams bufferpool pages
//! out to workloads.
//!
//! This module is the symmetric twin of [`crate::backend`]: where a
//! `Backend` *fetches* bytes from an origin into bufferpool pages, a
//! [`Frontend`] *serves* bufferpool pages out to a workload. The
//! shapes mirror each other on purpose so the shard bring-up can build
//! and drive both the same way.
//!
//! ## The `Frontend` / `Driver` seam
//!
//! A `Frontend` is a *factory*: [`Frontend::start_on_shard`] binds the
//! listener on one shard (with `SO_REUSEPORT`, so every shard accepts
//! the same port) and returns a [`Driver`]. The driver is what the
//! per-shard [`ShardLoop`](crate::runtime::ShardLoop) advances.
//!
//! [`Driver`] exposes a single non-async [`Driver::progress`] returning
//! whether it did work this tick. That is exactly the
//! [`ShardLoop::add_tick_hook`](crate::runtime::ShardLoop::add_tick_hook)
//! contract, so wiring is `loop.add_tick_hook(move || driver.progress())`.
//! A tick hook (rather than a single long-lived future) is the right
//! seam because a frontend multiplexes *many* connection futures whose
//! lifetimes do not line up with one `poll`; the driver owns that
//! connection set internally and the loop just keeps poking it. The
//! socket ring's own `progress()` is registered as a separate tick hook
//! by the shard bring-up, so the driver's `progress` only has to
//! advance its accept/parse/serve futures, not the ring.
//!
//! ## Registry
//!
//! [`FrontendRegistry`] mirrors `DiskRegistry`/the future
//! `BackendRegistry`: a string-keyed set of frontends built from
//! [`FrontendSpec`](crate::config::FrontendSpec)s, with add/remove so
//! `config::reconcile` can drive hot-reload (see
//! `config::reconcile::FrontendReconciler`). It is intentionally
//! decoupled from the runtime wiring: building a registry validates and
//! constructs the frontend objects; `start_on_shard` is what actually
//! binds sockets, and that happens later on each shard thread.

mod range;

#[cfg(target_os = "linux")]
mod http_serve;

use std::collections::HashMap;
use std::sync::Arc;

use crate::config::FrontendSpec;

pub use range::{ByteRange, RangeError, ResolvedRange, StripeSlice, full_object, stripe_set};

#[cfg(target_os = "linux")]
pub use http_serve::{HttpDriver, HttpFrontend};

/// A workload-facing listener that serves cached objects out of the
/// shard's bufferpool. Sibling to [`crate::backend::Backend`].
///
/// A `Frontend` is a per-node factory: it is constructed once from a
/// [`FrontendSpec`], then [`Self::start_on_shard`] is invoked once per
/// shard to bind that shard's listener and produce a [`Driver`] the
/// shard loop advances. The `Ctx` associated type is the shard-local
/// resource bundle the factory needs (page geometry, the socket ring,
/// the pool); it is an associated type rather than a fixed
/// `ShardContext` so the pure trait stays cross-platform and a
/// Linux-only frontend can name the concrete, io_uring-bearing
/// context.
pub trait Frontend {
    /// Per-shard driver this frontend produces. Advanced by the shard
    /// loop via [`Driver::progress`].
    type Driver: Driver;

    /// Shard-local context required to start serving on a shard.
    type Ctx;

    /// Stable identifier, matching the [`FrontendSpec::id`] this
    /// frontend was built from. Used as the registry key.
    fn id(&self) -> &str;

    /// Bind this shard's listener and return its [`Driver`].
    ///
    /// Called once per shard, on that shard's pinned thread, with the
    /// shard's `ctx`. The returned driver owns the shard-local accept /
    /// parse / serve state; the caller registers
    /// [`Driver::progress`] as a [`ShardLoop`](crate::runtime::ShardLoop)
    /// tick hook.
    fn start_on_shard(&self, ctx: Self::Ctx) -> Result<Self::Driver, FrontendError>;
}

/// The per-shard, cooperatively-driven half of a [`Frontend`].
///
/// Designed to slot directly into
/// [`ShardLoop::add_tick_hook`](crate::runtime::ShardLoop::add_tick_hook):
/// the loop calls [`Self::progress`] once per iteration and treats a
/// `true` return as "stay hot, do not sleep". A driver owns its
/// connection set and any in-flight `pool.read` / SEND_ZC futures
/// internally; `progress` advances them by one step.
pub trait Driver {
    /// Advance the driver by one cooperative step: accept any pending
    /// connection, parse ready requests, push serve-out work. Returns
    /// whether it did observable work this tick (the shard loop uses
    /// this to decide whether to busy-poll or sleep).
    fn progress(&mut self) -> bool;

    /// Whether the driver has any outstanding work (open connections or
    /// in-flight futures). The shard loop can use this during shutdown
    /// to drain before exiting. Defaults to "always busy" for drivers
    /// that do not track idleness.
    fn is_idle(&self) -> bool {
        false
    }
}

/// Blanket impl so a boxed driver is still a driver, mirroring the
/// `Backend for Arc<T>` blanket impl: a registry that erases the
/// concrete driver type behind `Box<dyn Driver>` can still hand it to
/// the shard loop unchanged.
impl Driver for Box<dyn Driver> {
    fn progress(&mut self) -> bool {
        (**self).progress()
    }

    fn is_idle(&self) -> bool {
        (**self).is_idle()
    }
}

/// Blanket impl mirroring `Backend for Arc<T>`, so a `Frontend` factory
/// can be shared across shards by handing each shard an `Arc`-wrapped
/// clone. `start_on_shard` takes `&self`, so the shared factory is
/// read-only; per-shard mutable state lives in the returned `Driver`.
impl<T: Frontend + ?Sized> Frontend for Arc<T> {
    type Driver = T::Driver;
    type Ctx = T::Ctx;

    fn id(&self) -> &str {
        (**self).id()
    }

    fn start_on_shard(&self, ctx: Self::Ctx) -> Result<Self::Driver, FrontendError> {
        (**self).start_on_shard(ctx)
    }
}

/// Errors building or starting a frontend. Mirrors the
/// `&'static str`-flavored, cheaply-cloneable style of
/// [`crate::bufferpool::Error`] so it threads through the registry and
/// the reconcile path without boxing.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum FrontendError {
    /// The [`FrontendSpec`] referenced a [`FrontendKind`] this build
    /// does not support (e.g. an HTTP frontend on a non-Linux target,
    /// where the socket ring does not exist).
    UnsupportedKind(&'static str),
    /// Two specs shared the same `id`; ids must be unique within a
    /// registry.
    DuplicateId(String),
    /// The listener address in the spec could not be parsed/bound.
    BadBind(String),
    /// A generic configuration problem with a static message.
    BadConfig(&'static str),
}

impl std::fmt::Display for FrontendError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            FrontendError::UnsupportedKind(k) => write!(f, "unsupported frontend kind: {k}"),
            FrontendError::DuplicateId(id) => write!(f, "duplicate frontend id: {id}"),
            FrontendError::BadBind(b) => write!(f, "bad frontend bind address: {b}"),
            FrontendError::BadConfig(m) => write!(f, "frontend config error: {m}"),
        }
    }
}

impl std::error::Error for FrontendError {}

/// A string-keyed set of constructed frontends, built from
/// [`FrontendSpec`]s. Mirrors `DiskRegistry`: it owns the frontend
/// *factories* (not per-shard drivers) and supports add/remove so the
/// config reconcile loop can apply a desired spec set.
///
/// The registry is generic over the concrete frontend type `F` so it
/// stays cross-platform and testable: the runtime instantiates it with
/// a concrete [`Frontend`] impl, while unit tests instantiate it with
/// an in-module test frontend. A spec whose kind this build cannot
/// construct surfaces as [`FrontendError::UnsupportedKind`] from the
/// `build` callback the caller supplies.
pub struct FrontendRegistry<F: Frontend> {
    frontends: HashMap<String, F>,
}

impl<F: Frontend> FrontendRegistry<F> {
    pub fn new() -> Self {
        Self {
            frontends: HashMap::new(),
        }
    }

    /// Build a registry from `specs`, constructing each via `build`.
    ///
    /// `build` is the spec-to-frontend constructor (the runtime passes
    /// a concrete `from_spec`; tests pass a stub). Duplicate ids are
    /// rejected. The callback indirection keeps the registry itself
    /// free of any HTTP/io_uring knowledge, so it compiles and tests on
    /// every platform.
    pub fn from_specs<B>(specs: &[FrontendSpec], mut build: B) -> Result<Self, FrontendError>
    where
        B: FnMut(&FrontendSpec) -> Result<F, FrontendError>,
    {
        let mut reg = Self::new();
        for spec in specs {
            let f = build(spec)?;
            reg.add(f)?;
        }
        Ok(reg)
    }

    /// Insert a frontend, keyed by its [`Frontend::id`]. Rejects a
    /// duplicate id rather than silently overwriting.
    pub fn add(&mut self, frontend: F) -> Result<(), FrontendError> {
        let id = frontend.id().to_string();
        if self.frontends.contains_key(&id) {
            return Err(FrontendError::DuplicateId(id));
        }
        self.frontends.insert(id, frontend);
        Ok(())
    }

    /// Remove and return the frontend with `id`, if present.
    pub fn remove(&mut self, id: &str) -> Option<F> {
        self.frontends.remove(id)
    }

    /// Borrow the frontend with `id`, if present.
    pub fn get(&self, id: &str) -> Option<&F> {
        self.frontends.get(id)
    }

    pub fn len(&self) -> usize {
        self.frontends.len()
    }

    pub fn is_empty(&self) -> bool {
        self.frontends.is_empty()
    }

    /// Iterate the registered frontends in arbitrary order.
    pub fn iter(&self) -> impl Iterator<Item = &F> {
        self.frontends.values()
    }
}

impl<F: Frontend> Default for FrontendRegistry<F> {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{FrontendKind, FrontendSpec};

    /// A driver that reports a fixed amount of work then goes idle, so
    /// the `Driver`/`ShardLoop` tick-hook contract can be exercised
    /// without a socket ring. Mirrors `EchoBackend` in spirit.
    struct CountingDriver {
        remaining: u32,
    }

    impl Driver for CountingDriver {
        fn progress(&mut self) -> bool {
            if self.remaining > 0 {
                self.remaining -= 1;
                true
            } else {
                false
            }
        }

        fn is_idle(&self) -> bool {
            self.remaining == 0
        }
    }

    /// An in-module test `Frontend`: a cross-platform stand-in for a
    /// concrete frontend so the trait, registry, and driver seam can be
    /// tested everywhere (not just Linux).
    struct TestFrontend {
        id: String,
        steps: u32,
    }

    impl Frontend for TestFrontend {
        type Driver = CountingDriver;
        type Ctx = ();

        fn id(&self) -> &str {
            &self.id
        }

        fn start_on_shard(&self, _ctx: ()) -> Result<CountingDriver, FrontendError> {
            Ok(CountingDriver {
                remaining: self.steps,
            })
        }
    }

    fn spec(id: &str) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: FrontendKind::Http,
            bind: "0.0.0.0:9000".to_string(),
            backend: "b".to_string(),
            tls: None,
        }
    }

    #[test]
    fn registry_builds_from_specs_and_keys_by_id() {
        let specs = [spec("a"), spec("b")];
        let reg = FrontendRegistry::from_specs(&specs, |s| {
            Ok(TestFrontend {
                id: s.id.clone(),
                steps: 2,
            })
        })
        .unwrap();
        assert_eq!(reg.len(), 2);
        assert!(reg.get("a").is_some());
        assert!(reg.get("b").is_some());
        assert!(reg.get("missing").is_none());
    }

    #[test]
    fn registry_rejects_duplicate_id() {
        let specs = [spec("dup"), spec("dup")];
        let res = FrontendRegistry::from_specs(&specs, |s| {
            Ok(TestFrontend {
                id: s.id.clone(),
                steps: 1,
            })
        });
        assert!(matches!(res, Err(FrontendError::DuplicateId(ref id)) if id == "dup"));
    }

    #[test]
    fn registry_propagates_build_error() {
        let specs = [spec("a")];
        let res = FrontendRegistry::<TestFrontend>::from_specs(&specs, |_| {
            Err(FrontendError::UnsupportedKind("test"))
        });
        assert!(matches!(res, Err(FrontendError::UnsupportedKind("test"))));
    }

    #[test]
    fn registry_add_remove() {
        let mut reg = FrontendRegistry::new();
        reg.add(TestFrontend {
            id: "x".into(),
            steps: 0,
        })
        .unwrap();
        assert_eq!(reg.len(), 1);
        // Duplicate add is rejected.
        assert!(
            reg.add(TestFrontend {
                id: "x".into(),
                steps: 0
            })
            .is_err()
        );
        assert!(reg.remove("x").is_some());
        assert!(reg.is_empty());
        assert!(reg.remove("x").is_none());
    }

    #[test]
    fn driver_progress_maps_onto_tick_hook_semantics() {
        let frontend = TestFrontend {
            id: "f".into(),
            steps: 3,
        };
        let mut driver = frontend.start_on_shard(()).unwrap();
        // Three busy ticks, then idle: exactly the busy/idle contract
        // the ShardLoop tick hook relies on.
        assert!(driver.progress());
        assert!(driver.progress());
        assert!(driver.progress());
        assert!(!driver.progress());
        assert!(driver.is_idle());
    }

    #[test]
    fn boxed_driver_blanket_impl() {
        let mut boxed: Box<dyn Driver> = Box::new(CountingDriver { remaining: 1 });
        assert!(boxed.progress());
        assert!(!boxed.progress());
        assert!(boxed.is_idle());
    }
}
