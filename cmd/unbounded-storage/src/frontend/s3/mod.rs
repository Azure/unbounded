// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The S3 frontend: a thin factory plus its [`ServePolicy`].
//!
//! This module is the S3 sibling of [`super::http`]. It owns:
//!
//! 1. **The factory** ([`S3Frontend`]): validates an `s3`
//!    [`FrontendSpec`](crate::config::FrontendSpec), parses the bind
//!    address, and remembers the catalog path. Like [`HttpFrontend`]
//!    (super::http::HttpFrontend) it does not implement the `Frontend`
//!    trait: the concrete bufferpool type is only nameable in the
//!    binary, so the binary binds the listener, loads the catalog, and
//!    constructs the driver directly.
//! 2. **The policy** ([`S3Policy`], in [`policy`]): the
//!    protocol-specific behavior - routing a request to a catalog
//!    object, resolving its range, and shaping S3 response heads and
//!    XML errors.
//!
//! The serving engine (accept loop, head parsing, zero-copy body
//! streaming) is shared with the HTTP frontend in [`super::conn`]; the
//! per-shard driver is the generic [`ServeDriver`](super::conn::ServeDriver)
//! specialized to [`S3Policy`], re-exported here as [`S3Driver`].
//!
//! A first-byte pool miss on a GET is handled entirely by the shared
//! engine: the `200` head has already been written, so the failed
//! body read just drops the connection (a truncated response), which
//! is the agreed v0 behavior.
//!
//! Linux-gated because serving depends on the io_uring
//! [`NetHandle`](crate::ring::NetHandle).

mod catalog;
mod policy;
mod response;
mod routing;

use std::net::SocketAddr;
use std::os::fd::RawFd;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use crate::config::{FrontendKind, FrontendSpec};
use crate::frontend::FrontendError;
use crate::frontend::conn::{ServeDriver, bind_listener};

pub use catalog::{ObjectMeta, YamlCatalog};
pub use policy::S3Policy;

/// Per-shard S3 serving driver: the shared [`ServeDriver`] engine
/// specialized to [`S3Policy`]. The concrete bufferpool type `P` is
/// only nameable in the binary, which constructs this directly with an
/// `Rc<S3Policy>`.
pub type S3Driver<P> = ServeDriver<P, S3Policy>;

/// S3 serving frontend factory. Built once per [`FrontendSpec`]; holds
/// the immutable configuration distilled from the spec, including the
/// path to the object catalog.
///
/// Like [`HttpFrontend`](super::http::HttpFrontend) this is called
/// directly by the binary rather than via a `Frontend` trait impl: the
/// per-shard [`S3Driver`] is generic over the concrete bufferpool type.
/// The binary loads the catalog once via [`Self::load_catalog`], shares
/// the resulting `Arc<YamlCatalog>` across shards, binds the listener
/// via [`Self::bind_listener`], and builds an [`S3Policy`] per shard.
pub struct S3Frontend {
    id: String,
    bind: SocketAddr,
    backend_id: String,
    catalog_path: PathBuf,
}

impl S3Frontend {
    /// Construct from a [`FrontendSpec`], validating the kind, parsing
    /// the bind address, and requiring the `catalog` path that load-time
    /// config validation guarantees for `s3` frontends.
    pub fn from_spec(spec: &FrontendSpec) -> Result<Self, FrontendError> {
        if spec.kind != FrontendKind::S3 {
            return Err(FrontendError::UnsupportedKind("non-s3 frontend kind"));
        }
        let bind = spec
            .bind
            .parse::<SocketAddr>()
            .map_err(|_| FrontendError::BadBind(spec.bind.clone()))?;
        let catalog_path = spec.catalog.clone().ok_or(FrontendError::BadConfig(
            "s3 frontend requires a catalog path",
        ))?;
        Ok(Self {
            id: spec.id.clone(),
            bind,
            backend_id: spec.backend.clone(),
            catalog_path,
        })
    }

    /// Stable identifier, matching [`FrontendSpec::id`].
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The backend id this frontend routes stripe-fetch misses through.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// The configured listen address.
    pub fn bind(&self) -> SocketAddr {
        self.bind
    }

    /// The configured catalog manifest path.
    pub fn catalog_path(&self) -> &Path {
        &self.catalog_path
    }

    /// Load the YAML object catalog from [`Self::catalog_path`] into a
    /// shareable `Arc`. Called once by the binary before the shards fan
    /// out; the returned `Arc` is cloned into each shard's [`S3Policy`].
    /// The error is the human-readable catalog parse/load message.
    pub fn load_catalog(&self) -> Result<Arc<YamlCatalog>, String> {
        YamlCatalog::load(&self.catalog_path).map(Arc::new)
    }

    /// Create, bind, and listen the per-shard accept socket with
    /// `SO_REUSEPORT` so every shard accepts on the same port,
    /// returning the listening [`RawFd`]. Linux-only.
    ///
    /// The caller owns the returned fd and is responsible for closing
    /// it; [`S3Driver`] only reads it for `accept`.
    pub fn bind_listener(&self) -> Result<RawFd, FrontendError> {
        bind_listener(self.bind)
    }
}

#[cfg(test)]
mod tests {
    use std::io::Write;

    use super::*;
    use crate::config::TlsCfg;

    fn spec(id: &str, bind: &str, catalog: Option<&str>) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: FrontendKind::S3,
            bind: bind.to_string(),
            backend: "primary".to_string(),
            catalog: catalog.map(PathBuf::from),
            tls: None,
        }
    }

    #[test]
    fn from_spec_validates_kind_bind_and_catalog() {
        let f = S3Frontend::from_spec(&spec("s3", "0.0.0.0:8080", Some("/etc/cat.yaml"))).unwrap();
        assert_eq!(f.id(), "s3");
        assert_eq!(f.backend_id(), "primary");
        assert_eq!(f.bind(), "0.0.0.0:8080".parse().unwrap());
        assert_eq!(f.catalog_path(), Path::new("/etc/cat.yaml"));

        let bad = S3Frontend::from_spec(&spec("f", "not-an-addr", Some("/c.yaml")));
        assert!(matches!(bad, Err(FrontendError::BadBind(_))));
    }

    #[test]
    fn from_spec_rejects_non_s3_kind() {
        let mut s = spec("f", "127.0.0.1:8080", Some("/c.yaml"));
        s.kind = FrontendKind::Http;
        assert!(matches!(
            S3Frontend::from_spec(&s),
            Err(FrontendError::UnsupportedKind(_))
        ));
    }

    #[test]
    fn from_spec_rejects_missing_catalog() {
        assert!(matches!(
            S3Frontend::from_spec(&spec("f", "127.0.0.1:8080", None)),
            Err(FrontendError::BadConfig(_))
        ));
    }

    #[test]
    fn from_spec_accepts_tls_block_but_ignores_it() {
        let mut s = spec("f", "127.0.0.1:8080", Some("/c.yaml"));
        s.tls = Some(TlsCfg {
            cert_path: None,
            key_path: None,
            secret_ref: Some("k8s://ns/name".into()),
        });
        assert!(S3Frontend::from_spec(&s).is_ok());
    }

    #[test]
    fn load_catalog_reads_manifest() {
        let mut f = tempfile::NamedTempFile::new().expect("tempfile");
        writeln!(
            f,
            "objects:\n  - bucket: demo\n    key: k\n    stripe: {}\n    size: 7",
            "aa".repeat(32)
        )
        .expect("write catalog");
        f.flush().expect("flush");

        let frontend = S3Frontend::from_spec(&spec(
            "s3",
            "0.0.0.0:8080",
            Some(f.path().to_str().unwrap()),
        ))
        .unwrap();
        let catalog = frontend.load_catalog().expect("catalog loads");
        let meta = catalog.get("demo", "k").expect("entry present");
        assert_eq!(meta.size, 7);
    }

    #[test]
    fn load_catalog_surfaces_error_for_missing_file() {
        let frontend =
            S3Frontend::from_spec(&spec("s3", "0.0.0.0:8080", Some("/no/such/catalog.yaml")))
                .unwrap();
        assert!(frontend.load_catalog().is_err());
    }
}
