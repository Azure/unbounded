// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Deterministic desired-state core. Adapters own I/O, authorization, and CAS.

pub mod kubernetes;
pub mod model;
#[cfg(test)]
extern crate self as racer_controlplane;
#[path = "../../../internal/racer/product.rs"]
pub mod product;
mod product_topology;
#[cfg(test)]
#[path = "../tests/support/publication.rs"]
mod publication;
pub mod security;
pub mod service;
pub mod status;
pub mod storage;
pub mod subscription;
pub mod topology;

pub mod proto {
    include!(concat!(env!("OUT_DIR"), "/racer.control.v1.rs"));
}

#[derive(Debug, thiserror::Error)]
#[error("{0}")]
pub struct Error(pub String);

pub type Result<T> = std::result::Result<T, Error>;
