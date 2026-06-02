// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The S3 object catalog: a static `(bucket, key) -> ObjectMeta` map
//! loaded from a YAML manifest at startup.
//!
//! This is the S3 frontend's analogue of the HTTP frontend's origin
//! `HEAD` lookup: where HTTP resolves an object's length (and identity)
//! from a live origin and caches it per-shard, S3 resolves it from this
//! immutable, pre-loaded manifest. In v0 each catalog entry maps to a
//! single stripe.

mod types;
mod yaml;

pub use types::ObjectMeta;
pub use yaml::YamlCatalog;
